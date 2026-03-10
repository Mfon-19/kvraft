package common

import (
	"context"

	pb "kvraft/proto"
)

const (
	OpGet    = "get"
	OpPut    = "put"
	OpDelete = "delete"
)

func InvokeKV(ctx context.Context, client pb.KVServiceClient, req ClientRequest) (ClientResponse, error) {
	switch req.Type {
	case OpGet:
		resp, err := client.Get(ctx, &pb.KVRequest{Key: req.Key})
		if err != nil {
			return ClientResponse{}, err
		}
		return ClientResponse{Success: resp.Success, Value: resp.Value, Error: resp.Error, Leader: resp.Leader}, nil
	case OpPut:
		resp, err := client.Put(ctx, &pb.KVRequest{Key: req.Key, Value: req.Value})
		if err != nil {
			return ClientResponse{}, err
		}
		return ClientResponse{Success: resp.Success, Value: resp.Value, Error: resp.Error, Leader: resp.Leader}, nil
	case OpDelete:
		resp, err := client.Delete(ctx, &pb.KVRequest{Key: req.Key})
		if err != nil {
			return ClientResponse{}, err
		}
		return ClientResponse{Success: resp.Success, Value: resp.Value, Error: resp.Error, Leader: resp.Leader}, nil
	default:
		return ClientResponse{Success: false, Error: "unknown command"}, nil
	}
}
