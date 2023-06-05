package main

import (
	"context"
	"fmt"
	"math/rand"

	"github.com/redis/go-redis/v9"
)

func main() {
	ctx := context.Background()

	rdb := redis.NewClient(&redis.Options{
		Addr: ":6379",
	})
	_ = rdb.FlushDB(ctx).Err()

	fmt.Printf("# BLOOM\n")
	bloomFilter(ctx, rdb)

	fmt.Printf("\n# CUCKOO\n")
	cuckooFilter(ctx, rdb)

	fmt.Printf("\n# COUNT-MIN\n")
	countMinSketch(ctx, rdb)

	fmt.Printf("\n# TOP-K\n")
	topK(ctx, rdb)
}

// 布隆过滤器
func bloomFilter(ctx context.Context, rdb *redis.Client) {
	// NOTE 可能因为不是redis的核心命令，而是布隆过滤器模块的命令，所以该命令加了一个BF前缀
	inserted, err := rdb.Do(ctx, "BF.ADD", "bf_key", "item0").Bool()
	if err != nil {
		panic(err)
	}
	if inserted {
		fmt.Println("item0 was inserted")
	} else {
		fmt.Println("item0 already exists")
	}

	for _, item := range []string{"item0", "item1"} {
		exists, err := rdb.Do(ctx, "BF.EXISTS", "bf_key", item).Bool()
		if err != nil {
			panic(err)
		}
		if exists {
			fmt.Printf("%s does exist\n", item)
		} else {
			fmt.Printf("%s does not exist\n", item)
		}
	}

	bools, err := rdb.Do(ctx, "BF.MADD", "bf_key", "item1", "item2", "item3").BoolSlice()
	if err != nil {
		panic(err)
	}
	fmt.Println("adding multiple items:", bools)
}

// HyperLogLog 数据结构。HyperLogLog 是一种基数估计算法，用于快速、高效地统计一个集合中不同元素的数量，即去重后的数量
func cuckooFilter(ctx context.Context, rdb *redis.Client) {
	inserted, err := rdb.Do(ctx, "CF.ADDNX", "cf_key", "item0").Bool()
	if err != nil {
		panic(err)
	}
	if inserted {
		fmt.Println("item0 was inserted")
	} else {
		fmt.Println("item0 already exists")
	}

	for _, item := range []string{"item0", "item1"} {
		exists, err := rdb.Do(ctx, "CF.EXISTS", "cf_key", item).Bool()
		if err != nil {
			panic(err)
		}
		if exists {
			fmt.Printf("%s does exist\n", item)
		} else {
			fmt.Printf("%s does not exist\n", item)
		}
	}

	deleted, err := rdb.Do(ctx, "CF.DEL", "cf_key", "item0").Bool()
	if err != nil {
		panic(err)
	}
	if deleted {
		fmt.Println("item0 was deleted")
	}
}

// Redis 的 CMS（Count-Min Sketch）是一种概率性的数据结构，用于快速统计字符串出现的次数。
// 在 Redis 中，CMS 通常用于实现热门元素统计、流量限制等功能。
//
// CMS 的原理比较简单。它由一个固定大小的二维数组和若干个哈希函数组成。
// 每个哈希函数将输入的字符串映射到二维数组的某个位置，并将该位置上的计数器加 1。
// 当需要统计某个字符串出现的次数时，可以通过多次哈希得到该字符串在各个位置上的计数器值，并取其中的最小值作为估计值。
//
// 由于 CMS 的算法特性，它的空间占用与要统计的元素数量和误差率有关。
// 在实际使用中，可以根据具体情况调整参数以平衡精度和空间占用的关系。
//
// 在 Redis 中，CMS 可以通过以下命令进行操作：
//
// CMS.INITIALIZE：初始化一个 CMS。
// CMS.INCRBY：增加指定字符串的计数器。
// CMS.MERGE：将多个 CMS 合并成一个 CMS。
// CMS.INFO：获取 CMS 的详细信息，例如哈希函数数量、数组大小、误差率等。
func countMinSketch(ctx context.Context, rdb *redis.Client) {
	// 精度为0.001，置信度为0.01
	if err := rdb.Do(ctx, "CMS.INITBYPROB", "count_min", 0.001, 0.01).Err(); err != nil {
		panic(err)
	}

	items := []string{"item1", "item2", "item3", "item4", "item5"}
	counts := make(map[string]int, len(items))

	for i := 0; i < 10000; i++ {
		n := rand.Intn(len(items))
		item := items[n]

		if err := rdb.Do(ctx, "CMS.INCRBY", "count_min", item, 1).Err(); err != nil {
			panic(err)
		}
		counts[item]++
	}

	for item, count := range counts {
		ns, err := rdb.Do(ctx, "CMS.QUERY", "count_min", item).Int64Slice()
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s: count-min=%d actual=%d\n", item, ns[0], count)
	}
}

// TMD，竟然连topK都有实现
func topK(ctx context.Context, rdb *redis.Client) {
	if err := rdb.Do(ctx, "TOPK.RESERVE", "top_items", 3).Err(); err != nil {
		panic(err)
	}

	counts := map[string]int{
		"item1": 1000,
		"item2": 2000,
		"item3": 3000,
		"item4": 4000,
		"item5": 5000,
		"item6": 6000,
	}

	for item, count := range counts {
		for i := 0; i < count; i++ {
			if err := rdb.Do(ctx, "TOPK.INCRBY", "top_items", item, 1).Err(); err != nil {
				panic(err)
			}
		}
	}

	items, err := rdb.Do(ctx, "TOPK.LIST", "top_items").StringSlice()
	if err != nil {
		panic(err)
	}

	for _, item := range items {
		ns, err := rdb.Do(ctx, "TOPK.COUNT", "top_items", item).Int64Slice()
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s: top-k=%d actual=%d\n", item, ns[0], counts[item])
	}
}
