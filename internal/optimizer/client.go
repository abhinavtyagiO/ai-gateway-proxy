package optimizer

import (
	"context"
	"time"

	gateway "github.com/abhinavtyagiO/ai-gateway-proxy/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultOptimizerAddr = "localhost:50051"
	optimizeTimeout      = 200 * time.Millisecond
)

type OptimizerClient struct {
	client gateway.OptimizerClient
	conn   *grpc.ClientConn
}

func NewClient(addr string) (*OptimizerClient, error) {
	if addr == "" {
		addr = defaultOptimizerAddr
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}

	return &OptimizerClient{
		client: gateway.NewOptimizerClient(conn),
		conn:   conn,
	}, nil
}

func (c *OptimizerClient) Optimize(ctx context.Context, prompt, model, userID, orgID string) (*gateway.OptimizationResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, optimizeTimeout)
	defer cancel()

	return c.client.OptimizePrompt(callCtx, &gateway.OptimizationRequest{
		Prompt:         prompt,
		ModelRequested: model,
		UserId:         userID,
		OrgId:          orgID,
	})
}

func (c *OptimizerClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}

	return c.conn.Close()
}
