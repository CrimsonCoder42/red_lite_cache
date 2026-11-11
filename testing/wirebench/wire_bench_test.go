// Covers: gRPC wire cost (grpc_build_2026-11-11, Phase 2C)
//
// The benchmarks that compared WebSocket with gRPC before WebSocket was removed (D-048; the
// comparison is doc/grpc_build_2026-11-11/out/bench_ws_vs_grpc.txt), now on gRPC only, kept as a
// regression baseline. Run testing/wirebench/run.sh (6 runs, medians). Bytes on the wire are counted
// by a TCP proxy between the client and the server, both directions.
package wirebench

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

func TestMain(m *testing.M) {
	logger.L = logger.New(io.Discard, slog.LevelError, false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

var wires = []string{"grpc"}

func leader(b *testing.B, wire string, mods ...func(*config.Config)) *integration.Server {
	return integration.StartLeader(b, append([]func(*config.Config){func(c *config.Config) {
		c.ConnSendQueue = 4096
		c.CacheMaxKeysPerSheet = 0
	}}, mods...)...)
}

func url(_, addr string) string { return addr }

func newReader(b *testing.B, wire, addr, sheet string) *reader.Reader {
	return integration.NewReaderAt(b, url(wire, addr), "", sheet, func(c *reader.Config) {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		c.ConnectTimeout = 60 * time.Second
		c.WriteTimeout = 30 * time.Second
		c.MaxEntries = 1 << 20
	})
}

// liveCounter counts the bytes read through it, as they pass.
type liveCounter struct {
	net.Conn
	n *atomic.Int64
}

func (l liveCounter) Read(p []byte) (int, error) {
	n, err := l.Conn.Read(p)
	l.n.Add(int64(n))
	return n, err
}

// newLiveCounter is a TCP proxy to target that counts every byte both ways.
func newLiveCounter(b *testing.B, target string) (string, *atomic.Int64) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	var total atomic.Int64
	b.Cleanup(func() { ln.Close() })
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				in.Close()
				continue
			}
			go func() { io.Copy(out, liveCounter{in, &total}); out.Close() }()
			go func() { io.Copy(in, liveCounter{out, &total}); in.Close() }()
			b.Cleanup(func() { in.Close(); out.Close() })
		}
	}()
	return ln.Addr().String(), &total
}

// BenchmarkOp: one acknowledged set, delete, and one-match delete_prefix, ns/op and wire bytes/op.
func BenchmarkOp(b *testing.B) {
	for _, op := range []string{"set", "delete", "prefix1"} {
		for _, wire := range wires {
			b.Run(op+"/"+wire, func(b *testing.B) {
				srv := leader(b, wire)
				addr, wireBytes := newLiveCounter(b, srv.Addr)
				name := integration.UniqueSheet(b)
				r := newReader(b, wire, addr, name)
				ctx := context.Background()
				keys := make([]string, b.N)
				for i := range keys {
					keys[i] = fmt.Sprintf("bench:%d:k", i)
				}
				if op != "set" {
					for _, k := range keys {
						if err := r.Set(ctx, k, 1); err != nil {
							b.Fatal(err)
						}
					}
				}
				time.Sleep(20 * time.Millisecond)
				start := wireBytes.Load()
				b.ResetTimer()
				for i := range b.N {
					var err error
					switch op {
					case "set":
						err = r.Set(ctx, keys[i], i)
					case "delete":
						err = r.Delete(ctx, keys[i])
					case "prefix1":
						err = r.DeletePrefix(ctx, fmt.Sprintf("bench:%d:", i))
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				time.Sleep(20 * time.Millisecond)
				b.ReportMetric(float64(wireBytes.Load()-start)/float64(b.N), "wire-B/op")
			})
		}
	}
}

// BenchmarkConnectState: a reader connecting to a sheet of 10k / 100k keys (New returns once the
// state is loaded), ns per connect and wire bytes per connect.
func BenchmarkConnectState(b *testing.B) {
	for _, n := range []int{10_000, 100_000} {
		for _, wire := range wires {
			b.Run(fmt.Sprintf("%dk/%s", n/1000, wire), func(b *testing.B) {
				srv := leader(b, wire)
				name := integration.UniqueSheet(b)
				w := newReader(b, wire, srv.Addr, name)
				fill(b, w, n)
				addr, wireBytes := newLiveCounter(b, srv.Addr)
				start := wireBytes.Load()
				b.ResetTimer()
				for range b.N {
					r, err := reader.New(reader.Config{LeaderAddr: url(wire, addr), Sheet: name, Password: "pw",
						ConnectTimeout: 60 * time.Second, MaxEntries: 1 << 20,
						Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
					if err != nil {
						b.Fatal(err)
					}
					if !r.Has("fill:0") {
						b.Fatal("state not loaded")
					}
					b.StopTimer()
					r.Close()
					b.StartTimer()
				}
				b.StopTimer()
				time.Sleep(20 * time.Millisecond)
				b.ReportMetric(float64(wireBytes.Load()-start)/float64(b.N), "wire-B/op")
			})
		}
	}
}

// fill sets n keys through w with up to 64 writes in flight.
func fill(b *testing.B, w *reader.Reader, n int) {
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	var failed atomic.Value
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := w.Set(context.Background(), fmt.Sprintf("fill:%d", i), map[string]any{"i": i, "pad": "pppppppppppppppppppppppppppppppp"}); err != nil {
				failed.Store(err)
			}
		})
	}
	wg.Wait()
	if err, ok := failed.Load().(error); ok {
		b.Fatal(err)
	}
}

// BenchmarkBroadcast: one acknowledged write until every one of N readers holds it (3 and 30).
func BenchmarkBroadcast(b *testing.B) {
	for _, n := range []int{3, 30} {
		for _, wire := range wires {
			b.Run(fmt.Sprintf("%d/%s", n, wire), func(b *testing.B) {
				srv := leader(b, wire)
				name := integration.UniqueSheet(b)
				w := newReader(b, wire, srv.Addr, name)
				rs := make([]*reader.Reader, n)
				for i := range rs {
					rs[i] = newReader(b, wire, srv.Addr, name)
				}
				b.ResetTimer()
				for i := range b.N {
					k := fmt.Sprintf("b%d", i)
					if err := w.Set(context.Background(), k, i); err != nil {
						b.Fatal(err)
					}
					for _, r := range rs {
						for !r.Has(k) {
							// spin: the broadcast is in flight
						}
					}
				}
			})
		}
	}
}

// BenchmarkParallel: 16 writers and 16 readers on one sheet; ns per write (wall / writes).
func BenchmarkParallel(b *testing.B) {
	for _, wire := range wires {
		b.Run(wire, func(b *testing.B) {
			srv := leader(b, wire)
			name := integration.UniqueSheet(b)
			ws := make([]*reader.Reader, 16)
			for i := range ws {
				ws[i] = newReader(b, wire, srv.Addr, name)
			}
			rs := make([]*reader.Reader, 16)
			for i := range rs {
				rs[i] = newReader(b, wire, srv.Addr, name)
			}
			stop := make(chan struct{})
			var reads atomic.Int64
			var wg sync.WaitGroup
			for _, r := range rs {
				wg.Go(func() {
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						default:
						}
						r.Has(fmt.Sprintf("p%d", i%1024))
						reads.Add(1)
					}
				})
			}
			var next atomic.Int64
			b.ResetTimer()
			var wwg sync.WaitGroup
			for _, w := range ws {
				wwg.Go(func() {
					for {
						i := next.Add(1)
						if i > int64(b.N) {
							return
						}
						if err := w.Set(context.Background(), fmt.Sprintf("p%d", i%1024), i); err != nil {
							b.Error(err)
							return
						}
					}
				})
			}
			wwg.Wait()
			b.StopTimer()
			close(stop)
			wg.Wait()
		})
	}
}

// BenchmarkStalledReader: one peer opens the sheet and never reads. ns/op is a healthy writer's
// acknowledged set while 3 healthy readers receive every broadcast; stalled-ended-ms is how long
// after the first broadcast the server ended the stalled peer (its queue full).
func BenchmarkStalledReader(b *testing.B) {
	for _, wire := range wires {
		b.Run(wire, func(b *testing.B) {
			srv := leader(b, wire, func(c *config.Config) { c.ConnSendQueue = 256 })
			name := integration.UniqueSheet(b)
			w := newReader(b, wire, srv.Addr, name)
			rs := make([]*reader.Reader, 3)
			for i := range rs {
				rs[i] = newReader(b, wire, srv.Addr, name)
			}
			before := srv.App.Conns()
			stall(b, wire, srv.Addr, name)
			for srv.App.Conns() != before+1 {
				time.Sleep(time.Millisecond)
			}
			value := make([]byte, 2048)
			for i := range value {
				value[i] = 'v'
			}
			sv := string(value)
			var ended time.Duration
			lat := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			start := time.Now()
			for i := range b.N {
				t0 := time.Now()
				k := fmt.Sprintf("s%d", i)
				if err := w.Set(context.Background(), k, sv); err != nil {
					b.Fatal(err)
				}
				lat = append(lat, time.Since(t0))
				if ended == 0 && srv.App.Conns() == before {
					ended = time.Since(start)
				}
			}
			b.StopTimer()
			deadline := time.Now().Add(30 * time.Second)
			for ended == 0 && srv.App.Conns() != before && time.Now().Before(deadline) {
				w.Set(context.Background(), "push", sv)
				time.Sleep(time.Millisecond)
			}
			if ended == 0 {
				ended = time.Since(start)
			}
			slices.Sort(lat)
			b.ReportMetric(float64(lat[len(lat)*99/100].Microseconds()), "p99-us")
			b.ReportMetric(float64(ended.Milliseconds()), "stalled-ended-ms")
		})
	}
}

// stall opens sheet on a connection whose replies are never read.
func stall(b *testing.B, wire, addr, sheet string) {
	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { cc.Close() })
	s, err := pb.NewCacheClient(cc).Session(ctx)
	if err != nil {
		b.Fatal(err)
	}
	if err := s.Send(&pb.ClientMsg{Id: 1, Body: &pb.ClientMsg_Open{Open: &pb.Open{Sheet: sheet, Password: "pw"}}}); err != nil {
		b.Fatal(err)
	}
}
