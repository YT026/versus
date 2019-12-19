package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

type clientStats struct {
	mu         sync.Mutex
	NumTotal   int           // Number of requests
	NumErrors  int           // Number of errors
	TimeErrors time.Duration // Duration of error responses specifically
	TimeTotal  time.Duration // Total duration of requests
	Errors     map[string]int
}

func (stats *clientStats) Count(err error, elapsed time.Duration) {
	stats.mu.Lock()
	defer stats.mu.Unlock()

	stats.NumTotal += 1
	if err != nil {
		stats.NumErrors += 1
		stats.TimeErrors += elapsed

		if stats.Errors == nil {
			stats.Errors = map[string]int{}
		}
		stats.Errors[err.Error()] += 1
	}
	stats.TimeTotal += elapsed
}

func (stats *clientStats) Render(w io.Writer) error {
	if stats.NumTotal == 0 {
		fmt.Fprintf(w, "   No requests.")
	}
	var errRate, rps float64

	errRate = float64(stats.NumErrors*100) / float64(stats.NumTotal)
	rps = float64(stats.NumTotal) / stats.TimeTotal.Seconds()
	reqAvg := stats.TimeTotal / time.Duration(stats.NumTotal)

	fmt.Fprintf(w, "   Requests/Sec: %0.2f", rps)
	if stats.NumErrors > 0 && stats.NumErrors != stats.NumTotal {
		errAvg := stats.TimeErrors / time.Duration(stats.NumErrors)
		fmt.Fprintf(w, ", %s per error", errAvg)
	}
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "   Average:      %s\n", reqAvg)
	fmt.Fprintf(w, "   Errors:       %0.2f%%\n", errRate)

	for msg, num := range stats.Errors {
		fmt.Fprintf(w, "   * [%d] %q\n", num, msg)
	}

	return nil
}

func NewClient(endpoint string, concurrency int) (*Client, error) {
	c := Client{
		Endpoint:    endpoint,
		Concurrency: concurrency,
		In:          make(chan Request, 2*concurrency),
	}
	return &c, nil
}

func NewClients(endpoints []string, concurrency int, timeout time.Duration) (Clients, error) {
	clients := make(Clients, 0, len(endpoints))
	for _, endpoint := range endpoints {
		c, err := NewClient(endpoint, concurrency)
		if err != nil {
			return nil, err
		}
		c.Timeout = timeout
		clients = append(clients, c)
	}
	return clients, nil
}

type Client struct {
	Endpoint    string
	Concurrency int           // Number of goroutines to make requests with. Must be >=1.
	Timeout     time.Duration // Timeout of each request

	In    chan Request
	Stats clientStats
}

func (client *Client) Handle(req Request) {
	client.In <- req
}

// Serve starts the async request and response goroutine consumers.
func (client *Client) Serve(ctx context.Context, out chan<- Response) error {
	g, ctx := errgroup.WithContext(ctx)

	if client.Concurrency < 1 {
		client.Concurrency = 1
	}

	logger.Debug().Str("endpoint", client.Endpoint).Int("concurrency", client.Concurrency).Msg("starting client")

	for i := 0; i < client.Concurrency; i++ {
		g.Go(func() error {
			// Consume requests
			t, err := NewTransport(client.Endpoint, client.Timeout)
			if err != nil {
				return err
			}
			for {
				select {
				case <-ctx.Done():
					logger.Debug().Str("endpoint", client.Endpoint).Msg("aborting client")
					return nil
				case req := <-client.In:
					if req.ID == -1 {
						// Final request received, shutdown
						logger.Debug().Str("endpoint", client.Endpoint).Msg("received final request, shutting down")
						return nil
					}
					resp := req.Do(t)
					client.Stats.Count(resp.Err, resp.Elapsed)
					select {
					case out <- resp:
					default:
						logger.Warn().Msg("response channel is overloaded, please open an issue")
						out <- resp
					}
				}
			}
		})
	}

	return g.Wait()
}

var id requestID

type Clients []*Client

// Finalize sends a request with ID -1 which signals the end of the stream, so
// serving will end cleanly.
func (c Clients) Finalize() {
	for _, client := range c {
		for i := 0; i < client.Concurrency; i++ {
			// Signal each client instance to shut down
			client.In <- Request{
				ID: -1,
			}
		}
	}
}

func (c Clients) Send(ctx context.Context, line []byte) error {
	id += 1
	for _, client := range c {
		select {
		case client.In <- Request{
			client: client,
			ID:     id,

			Line:      line,
			Timestamp: time.Now(),
		}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (clients Clients) Serve(ctx context.Context, out chan Response) error {
	g, ctx := errgroup.WithContext(ctx)

	for _, c := range clients {
		c := c // Otherwise c gets mutated per iteration and we get a race
		g.Go(func() error {
			return c.Serve(ctx, out)
		})
	}

	return g.Wait()
}