package client

import (
	"context"
	"fmt"
	"github.com/projectcalico/calico/goldmane/proto"
	"google.golang.org/grpc"
	"io"
)

type flowAPIClient struct {
	cli proto.FlowAPIClient
}

type FlowAPIClient interface {
	List(context.Context, *proto.FlowRequest) ([]*proto.Flow, error)
	Stream(ctx context.Context, request *proto.FlowRequest) (proto.FlowAPI_StreamClient, error)
}

func NewFlowsAPIClient(host string, opts ...grpc.DialOption) (FlowAPIClient, error) {
	gmCli, err := grpc.NewClient(host, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create grpc client:%w", err)
	}

	return &flowAPIClient{
		cli: proto.NewFlowAPIClient(gmCli),
	}, nil
}

func (cli *flowAPIClient) List(ctx context.Context, request *proto.FlowRequest) ([]*proto.Flow, error) {
	stream, err := cli.cli.List(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("failed to list flows: %w", err)
	}

	var flows []*proto.Flow
	for {
		flow, err := stream.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("failed to receive flow from stream: %w", err)
		}

		flows = append(flows, flow)
	}

	return flows, nil
}

// TODO wrap the stream client to make it easier to use?? Maybe make some generics around this?
func (cli *flowAPIClient) Stream(ctx context.Context, request *proto.FlowRequest) (proto.FlowAPI_StreamClient, error) {
	return cli.cli.Stream(ctx, request)
}
