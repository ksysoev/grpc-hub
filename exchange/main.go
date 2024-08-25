package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/ksysoev/oneway/api"
	"google.golang.org/grpc"
	"tailscale.com/net/socks5"
)

type ExchangeService struct {
	lock    sync.Mutex
	srvs    map[string]*Service
	pooller *ConnectionPooler
	api.UnimplementedExchangeServiceServer
}

func (s *ExchangeService) RegisterService(req *api.RegisterRequest, stream grpc.ServerStreamingServer[api.ConnectCommand]) error {
	commandChan := make(chan ConnectCommand)
	srv := NewService(stream.Context(), req.NameSpace, req.ServiceName, commandChan)

	s.lock.Lock()
	s.srvs[req.ServiceName] = srv
	s.lock.Unlock()

	slog.Info("Service registered", slog.String("namespace", req.NameSpace), slog.String("service", req.ServiceName))

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case cmd, ok := <-commandChan:
			if !ok {
				return nil
			}
			if cmd.Name != req.ServiceName {
				slog.Error("invalid service name", slog.String("name", cmd.Name), slog.String("expected", req.ServiceName))

				cmd.RespChan <- ConnectCommandResponse{Err: fmt.Errorf("service not found")}
				continue
			}

			s.pooller.WaitForConn(&cmd)

			err := stream.Send(&api.ConnectCommand{
				NameSpace:   cmd.NameSpace,
				ServiceName: cmd.Name,
				Id:          cmd.ID,
			})

			if err != nil {
				return fmt.Errorf("failed to send command: %w", err)
			}
		}
	}
}

func (s *ExchangeService) GetService(ctx context.Context, address string) (*Service, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	srv, ok := s.srvs[address]
	if !ok {
		for k, v := range s.srvs {
			slog.Info("Service", slog.String("key", k), slog.Any("value", v))
		}

		return nil, fmt.Errorf("service not found")
	}

	return srv, nil
}

// Exchange will be between the client and the server and will be routing and multiplexing client connections to the correct service
func main() {
	ctx := context.Background()
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
	defer cancel()

	errs := make(chan error, 1)

	go func() { errs <- startAPI(ctx) }()

	if err := <-errs; err != nil {
		slog.Error("Failed to start exchange service", slog.Any("error", err))
	}
}

func startAPI(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pooler := NewConnectionPooler(":9091")

	exchange := &ExchangeService{
		srvs:    make(map[string]*Service),
		pooller: pooler,
	}

	grpcServer := grpc.NewServer()
	api.RegisterExchangeServiceServer(grpcServer, exchange)

	socks5Server := socks5.Server{
		Dialer: func(ctx context.Context, network, address string) (net.Conn, error) {
			slog.Info("Dialing", slog.String("address", address))

			address, _, err := net.SplitHostPort(address)

			slog.Info("GetService", slog.String("address", address))

			if err != nil {
				return nil, fmt.Errorf("failed to split address: %w", err)
			}

			srv, err := exchange.GetService(ctx, address)
			if err != nil {
				slog.Error("Failed to get service", slog.Any("error", err))
				return nil, fmt.Errorf("failed to get service: %w", err)
			}

			conn, err := srv.RequestConn(ctx, address)
			if err != nil {
				slog.Error("Failed to request connection", slog.Any("error", err))
				return nil, fmt.Errorf("failed to request connection: %w", err)
			}

			slog.Info("Connection established", slog.String("address", address))
			return conn, nil
		},
	}

	lis, err := net.Listen("tcp", ":9090")
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	go func() {
		defer cancel()
		err := pooler.Run(ctx)
		if err != nil {
			slog.Error("Failed to run connection pooler", slog.Any("error", err))
		}
	}()

	go func() {
		defer cancel()

		lis, err := net.Listen("tcp", ":1080")
		if err != nil {
			slog.Error("Failed to listen", slog.Any("error", err))
			return
		}

		slog.Info("SOCKS5 server started", slog.Int64("port", 1080))

		go func() {
			<-ctx.Done()
			lis.Close()
		}()

		err = socks5Server.Serve(lis)
		if errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE) {
			return
		}

		if err != nil {
			slog.Error("Failed to serve", slog.Any("error", err))
		}
	}()

	slog.Info("Exchange service started", slog.Int64("port", 9090))
	err = grpcServer.Serve(lis)
	if err != nil {
		return fmt.Errorf("failed to serve: %w", err)
	}

	return nil
}

type connection struct {
	conn net.Conn
	id   uint64
}
