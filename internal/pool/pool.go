package pool

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9/internal"
)

var (
	// ErrClosed performs any operation on the closed client will return this error.
	ErrClosed = errors.New("redis: client is closed")

	// ErrPoolTimeout timed out waiting to get a connection from the connection pool.
	ErrPoolTimeout = errors.New("redis: connection pool timeout")
)

var timers = sync.Pool{
	New: func() interface{} {
		t := time.NewTimer(time.Hour)
		t.Stop()
		return t
	},
}

// Stats contains pool state information and accumulated stats.
type Stats struct {
	Hits     uint32 // number of times free connection was found in the pool
	Misses   uint32 // number of times free connection was NOT found in the pool
	Timeouts uint32 // number of times a wait timeout occurred

	TotalConns uint32 // number of total connections in the pool
	IdleConns  uint32 // number of idle connections in the pool
	StaleConns uint32 // number of stale connections removed from the pool
}

type Pooler interface {
	NewConn(context.Context) (*Conn, error)
	CloseConn(*Conn) error

	Get(context.Context) (*Conn, error)
	Put(context.Context, *Conn)
	Remove(context.Context, *Conn, error)

	Len() int
	IdleLen() int
	Stats() *Stats

	Close() error
}

type Options struct {
	Dialer func(context.Context) (net.Conn, error)

	PoolFIFO        bool // 取idleConn的conn时是否采用FIFO的方式
	PoolSize        int  // 连接池大小
	PoolTimeout     time.Duration
	MinIdleConns    int
	MaxIdleConns    int
	ConnMaxIdleTime time.Duration
	ConnMaxLifetime time.Duration
}

type lastDialErrorWrap struct {
	err error
}

type ConnPool struct {
	cfg *Options

	dialErrorsNum uint32 // atomic   拨号失败次数
	lastDialError atomic.Value

	queue chan struct{} // size=opt.PoolSize  等待队列，取conn时需要queue <- struct{}{}，成功则可以取conn

	connsMu   sync.Mutex
	conns     []*Conn // size=opt.PoolSize   所有连接
	idleConns []*Conn // size=opt.PoolSize   空闲连接

	poolSize     int // 当前的连接池大小
	idleConnsLen int // 当前的空闲连接数量

	stats Stats

	_closed uint32 // atomic   连接池是否已关闭，为1则是已关闭
}

var _ Pooler = (*ConnPool)(nil)

func NewConnPool(opt *Options) *ConnPool {
	p := &ConnPool{
		cfg: opt,

		queue:     make(chan struct{}, opt.PoolSize),
		conns:     make([]*Conn, 0, opt.PoolSize),
		idleConns: make([]*Conn, 0, opt.PoolSize),
	}

	p.connsMu.Lock()
	p.checkMinIdleConns()
	p.connsMu.Unlock()

	return p
}

// NOTE 不断循环重复拨号建立连接，直到现有线程池大小达到配置的大小，或者现有的空闲连接数达到配置的数量
func (p *ConnPool) checkMinIdleConns() {
	if p.cfg.MinIdleConns == 0 {
		return
	}
	for p.poolSize < p.cfg.PoolSize && p.idleConnsLen < p.cfg.MinIdleConns {
		select {
		case p.queue <- struct{}{}:
			p.poolSize++
			p.idleConnsLen++

			go func() {
				err := p.addIdleConn() // 拨号建立连接，加入conns和idleConns
				if err != nil && err != ErrClosed {
					p.connsMu.Lock()
					p.poolSize--
					p.idleConnsLen--
					p.connsMu.Unlock()
				}

				p.freeTurn() // <-p.queue
			}()
		default:
			return
		}
	}
}

func (p *ConnPool) addIdleConn() error {
	cn, err := p.dialConn(context.TODO(), true)
	if err != nil {
		return err
	}

	p.connsMu.Lock()
	defer p.connsMu.Unlock()

	// It is not allowed to add new connections to the closed connection pool.
	if p.closed() {
		_ = cn.Close()
		return ErrClosed
	}

	p.conns = append(p.conns, cn)
	p.idleConns = append(p.idleConns, cn)
	return nil
}

func (p *ConnPool) NewConn(ctx context.Context) (*Conn, error) {
	return p.newConn(ctx, false)
}

func (p *ConnPool) newConn(ctx context.Context, pooled bool) (*Conn, error) {
	cn, err := p.dialConn(ctx, pooled)
	if err != nil {
		return nil, err
	}

	p.connsMu.Lock()
	defer p.connsMu.Unlock()

	// It is not allowed to add new connections to the closed connection pool.
	if p.closed() {
		_ = cn.Close()
		return nil, ErrClosed
	}

	p.conns = append(p.conns, cn)
	if pooled {
		// If pool is full remove the cn on next Put.
		if p.poolSize >= p.cfg.PoolSize {
			cn.pooled = false
		} else {
			p.poolSize++
		}
	}

	return cn, nil
}

func (p *ConnPool) dialConn(ctx context.Context, pooled bool) (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}

	if atomic.LoadUint32(&p.dialErrorsNum) >= uint32(p.cfg.PoolSize) {
		return nil, p.getLastDialError()
	}

	netConn, err := p.cfg.Dialer(ctx)
	if err != nil {
		p.setLastDialError(err)
		if atomic.AddUint32(&p.dialErrorsNum, 1) == uint32(p.cfg.PoolSize) {
			go p.tryDial() // 循环直到连接建立成功，置空dialErrorsNum
		}
		return nil, err
	}

	cn := NewConn(netConn)
	cn.pooled = pooled
	return cn, nil
}

// for尝试拨号建立连接，直到连接建立成功之后将p.dialErrorsNum置为0，并将建立的连接关闭
// 所以这个方法的目的只是为了清空dialErrorsNum
func (p *ConnPool) tryDial() {
	for {
		if p.closed() {
			return
		}

		conn, err := p.cfg.Dialer(context.Background())
		if err != nil {
			p.setLastDialError(err)
			time.Sleep(time.Second)
			continue
		}

		atomic.StoreUint32(&p.dialErrorsNum, 0)
		_ = conn.Close()
		return
	}
}

func (p *ConnPool) setLastDialError(err error) {
	p.lastDialError.Store(&lastDialErrorWrap{err: err})
}

func (p *ConnPool) getLastDialError() error {
	err, _ := p.lastDialError.Load().(*lastDialErrorWrap)
	if err != nil {
		return err.err
	}
	return nil
}

// Get returns existed connection from the pool or creates a new one.
// 先尝试能否queue <- struct{}{}，可以说明可以get conn，不能就说明等待的太多了，返回err
// 之后从ConnPool取一个conn，取完看看是否需要补充，最后再校验下conn的状态，以及统计conn缓存的命中情况
// 若从池中拿不到，则新建一个conn
func (p *ConnPool) Get(ctx context.Context) (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}

	if err := p.waitTurn(ctx); err != nil {
		return nil, err
	}

	for {
		p.connsMu.Lock()
		cn, err := p.popIdle()
		p.connsMu.Unlock()

		if err != nil {
			return nil, err
		}

		if cn == nil {
			break
		}

		if !p.isHealthyConn(cn) {
			_ = p.CloseConn(cn)
			continue
		}

		atomic.AddUint32(&p.stats.Hits, 1)
		return cn, nil
	}

	atomic.AddUint32(&p.stats.Misses, 1)

	newcn, err := p.newConn(ctx, true)
	if err != nil {
		p.freeTurn()
		return nil, err
	}

	return newcn, nil
}

// 等待队列还有空间时，可以直接取conn；无空间则等待cfg.PoolTimeout，到期到返回err
func (p *ConnPool) waitTurn(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	select {
	case p.queue <- struct{}{}:
		return nil
	default:
	}

	timer := timers.Get().(*time.Timer)
	timer.Reset(p.cfg.PoolTimeout)

	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		timers.Put(timer)
		return ctx.Err()
	case p.queue <- struct{}{}:
		if !timer.Stop() {
			<-timer.C
		}
		timers.Put(timer)
		return nil
	case <-timer.C:
		timers.Put(timer)
		atomic.AddUint32(&p.stats.Timeouts, 1)
		return ErrPoolTimeout
	}
}

func (p *ConnPool) freeTurn() {
	<-p.queue
}

// 采用先进先出或先进后出的方法，从idleConns取一个conn，之后再调用checkMinIdleConns检查是否需要补充conn
func (p *ConnPool) popIdle() (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}
	n := len(p.idleConns)
	if n == 0 {
		return nil, nil
	}

	var cn *Conn
	if p.cfg.PoolFIFO {
		cn = p.idleConns[0]
		copy(p.idleConns, p.idleConns[1:])
		p.idleConns = p.idleConns[:n-1]
	} else {
		idx := n - 1
		cn = p.idleConns[idx]
		p.idleConns = p.idleConns[:idx]
	}
	p.idleConnsLen--
	p.checkMinIdleConns()
	return cn, nil
}

func (p *ConnPool) Put(ctx context.Context, cn *Conn) {
	if cn.rd.Buffered() > 0 {
		internal.Logger.Printf(ctx, "Conn has unread data")
		p.Remove(ctx, cn, BadConnError{})
		return
	}

	if !cn.pooled {
		p.Remove(ctx, cn, nil)
		return
	}

	var shouldCloseConn bool

	p.connsMu.Lock()

	if p.cfg.MaxIdleConns == 0 || p.idleConnsLen < p.cfg.MaxIdleConns {
		p.idleConns = append(p.idleConns, cn)
		p.idleConnsLen++
	} else {
		p.removeConn(cn)
		shouldCloseConn = true
	}

	p.connsMu.Unlock()

	p.freeTurn()

	if shouldCloseConn {
		_ = p.closeConn(cn)
	}
}

func (p *ConnPool) Remove(_ context.Context, cn *Conn, reason error) {
	p.removeConnWithLock(cn)
	p.freeTurn()
	_ = p.closeConn(cn)
}

func (p *ConnPool) CloseConn(cn *Conn) error {
	p.removeConnWithLock(cn)
	return p.closeConn(cn)
}

func (p *ConnPool) removeConnWithLock(cn *Conn) {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	p.removeConn(cn)
}

func (p *ConnPool) removeConn(cn *Conn) {
	for i, c := range p.conns {
		if c == cn {
			p.conns = append(p.conns[:i], p.conns[i+1:]...)
			if cn.pooled {
				p.poolSize--
				p.checkMinIdleConns()
			}
			break
		}
	}
	atomic.AddUint32(&p.stats.StaleConns, 1)
}

func (p *ConnPool) closeConn(cn *Conn) error {
	return cn.Close()
}

// Len returns total number of connections.
func (p *ConnPool) Len() int {
	p.connsMu.Lock()
	n := len(p.conns)
	p.connsMu.Unlock()
	return n
}

// IdleLen returns number of idle connections.
func (p *ConnPool) IdleLen() int {
	p.connsMu.Lock()
	n := p.idleConnsLen
	p.connsMu.Unlock()
	return n
}

func (p *ConnPool) Stats() *Stats {
	return &Stats{
		Hits:     atomic.LoadUint32(&p.stats.Hits),
		Misses:   atomic.LoadUint32(&p.stats.Misses),
		Timeouts: atomic.LoadUint32(&p.stats.Timeouts),

		TotalConns: uint32(p.Len()),
		IdleConns:  uint32(p.IdleLen()),
		StaleConns: atomic.LoadUint32(&p.stats.StaleConns),
	}
}

func (p *ConnPool) closed() bool {
	return atomic.LoadUint32(&p._closed) == 1
}

func (p *ConnPool) Filter(fn func(*Conn) bool) error {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()

	var firstErr error
	for _, cn := range p.conns {
		if fn(cn) {
			if err := p.closeConn(cn); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (p *ConnPool) Close() error {
	if !atomic.CompareAndSwapUint32(&p._closed, 0, 1) {
		return ErrClosed
	}

	var firstErr error
	p.connsMu.Lock()
	for _, cn := range p.conns {
		if err := p.closeConn(cn); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	p.conns = nil
	p.poolSize = 0
	p.idleConns = nil
	p.idleConnsLen = 0
	p.connsMu.Unlock()

	return firstErr
}

// 校验连接的最大生命周期和最大空闲时间，以及看看能否读出东西来，有点小6
func (p *ConnPool) isHealthyConn(cn *Conn) bool {
	now := time.Now()

	if p.cfg.ConnMaxLifetime > 0 && now.Sub(cn.createdAt) >= p.cfg.ConnMaxLifetime {
		return false
	}
	if p.cfg.ConnMaxIdleTime > 0 && now.Sub(cn.UsedAt()) >= p.cfg.ConnMaxIdleTime {
		return false
	}

	if connCheck(cn.netConn) != nil {
		return false
	}

	cn.SetUsedAt(now)
	return true
}
