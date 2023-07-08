// Copyright 2023 GoEdge CDN goedge.cdn@gmail.com. All rights reserved. Official site: https://goedge.cn .

package fsutils_test

import (
	fsutils "github.com/TeaOSLab/EdgeNode/internal/utils/fs"
	"sync"
	"testing"
	"time"
)

func TestStat(t *testing.T) {
	stat, err := fsutils.Stat("/usr/local")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("available:", stat.AvailableSize()/(1<<30), "total:", stat.TotalSize()/(1<<30), "used:", stat.UsedSize()/(1<<30))
}

func TestStatCache(t *testing.T) {
	for i := 0; i < 10; i++ {
		stat, err := fsutils.StatCache("/usr/local")
		if err != nil {
			t.Fatal(err)
		}
		t.Log("available:", stat.AvailableSize()/(1<<30), "total:", stat.TotalSize()/(1<<30), "used:", stat.UsedSize()/(1<<30))
	}
}

func TestConcurrent(t *testing.T) {
	var before = time.Now()
	defer func() {
		t.Log(time.Since(before).Seconds()*1000, "ms")
	}()

	var count = 10000
	var wg = sync.WaitGroup{}
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()

			_, _ = fsutils.Stat("/usr/local")
		}()
	}
	wg.Wait()
}

func BenchmarkStat(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := fsutils.Stat("/usr/local")
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkStatCache(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := fsutils.StatCache("/usr/local")
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
