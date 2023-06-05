​	其实看下来，总的原理还蛮简单的。跟server交互基本流程：封装cmd(包含命令和对应值的信息) -> 按照redis协议来封装这个cmd的信息，基本格式都是固定的啦 -> 通过conn发送封装好的数据 -> 得到返回后按照redis协议进行解析 -> 返回cmd，读取cmd的val得到结果

​	看👇🏻吧，基本都是这个b样，组装cmd，然后调用c

<img src="/Users/chb/Library/Application Support/typora-user-images/image-20230605224454721.png" alt="image-20230605224454721" style="zoom:50%;" />

​	这个c的方法定义，其内部调用的是process方法。当然啦，在redis-cli中这个process其实是被很多个中间件(hook)包起来的，hook执行完才执行process，或者hook也可以控制process的执行

​	我们可以看到，process中其实是调用了\_process啦，而process负责来控制重试操作。来到\_process中，可以看到它调用了WithWriter和WithReader两个方法，前一个就是写命令给redisServer啦，后一个就是读取server的响应内容，当然，这两个方法都包含了一堆的协议封装和转化，这个感兴趣的可以细看方法哈

​	那其实go-redis-cli的所有方法基本都是围绕Process来搞的，所以也没有啥大好看的啦

```go
func (c *Client) Process(ctx context.Context, cmd Cmder) error {
	err := c.processHook(ctx, cmd)
	cmd.SetErr(err)
	return err
}

// NOTE redis-client中最关键的方法
func (c *baseClient) process(ctx context.Context, cmd Cmder) error {
	var lastErr error
	for attempt := 0; attempt <= c.opt.MaxRetries; attempt++ {
		attempt := attempt

		retry, err := c._process(ctx, cmd, attempt)
		if err == nil || !retry {
			return err
		}

		lastErr = err
	}
	return lastErr
}

// NOTE 关键方法，主要是将cmd按redis协议组织后写入conn，再将相应的内容按协议读取出来，结果都塞到cmd的val中去
func (c *baseClient) _process(ctx context.Context, cmd Cmder, attempt int) (bool, error) {
	// 重试之间需要有一定的时间间隔，这里根据retryBackoff来计算
	if attempt > 0 {
		if err := internal.Sleep(ctx, c.retryBackoff(attempt)); err != nil {
			return false, err
		}
	}

	retryTimeout := uint32(0)

	if err := c.withConn(ctx, func(ctx context.Context, cn *pool.Conn) error { // 取conn，调func(ctx context.Context, cn *pool.Conn)
		if err := cn.WithWriter(c.context(ctx), c.opt.WriteTimeout, // 处理Deadline相关，并刷新writeCmd的写入
			func(wr *proto.Writer) error {
				return writeCmd(wr, cmd) // 按照redis协议组织args并进行写入
			}); err != nil {
			atomic.StoreUint32(&retryTimeout, 1)
			return err
		}
		// TODO readReply方法还没看
		if err := cn.WithReader(c.context(ctx), c.cmdTimeout(cmd), cmd.readReply); err != nil { // 处理超时相关，再调用readReply
			if cmd.readTimeout() == nil {
				atomic.StoreUint32(&retryTimeout, 1)
			} else {
				atomic.StoreUint32(&retryTimeout, 0)
			}
			return err
		}

		return nil
	}); err != nil {
		retry := shouldRetry(err, atomic.LoadUint32(&retryTimeout) == 1)
		return retry, err
	}

	return false, nil
}
```

