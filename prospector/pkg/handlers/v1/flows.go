package v1

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/goldmane/pkg/client"
	"github.com/projectcalico/calico/goldmane/proto"
	"github.com/projectcalico/calico/lib/httpapimachinery/pkg/handler"
	v1 "github.com/projectcalico/calico/prospector/pkg/apis/v1"
)

type flowsHdlr struct {
	gmCli client.FlowAPIClient
}

func NewFlows(cli client.FlowAPIClient) *flowsHdlr {
	return &flowsHdlr{cli}
}

func (hdlr *flowsHdlr) APIs() []handler.API {
	return []handler.API{
		{
			Method:  http.MethodGet,
			URL:     v1.FlowsPath,
			Handler: handler.NewBasicJSONHandler(hdlr.List),
		},
		{
			Method:  http.MethodGet,
			URL:     v1.FlowsStreamPath,
			Handler: handler.NewJSONStreamHandler(hdlr.Stream),
		},
	}
}

func (hdlr *flowsHdlr) List(ctx context.Context, params v1.ListFlowsParams) handler.ResponseType[[]v1.FlowResponse] {
	logrus.Info("list flows")

	flows, err := hdlr.gmCli.List(ctx, &proto.FlowRequest{})
	if err != nil {
		logrus.WithError(err).Error("failed to list flows")
		return handler.NewErrorResponse[[]v1.FlowResponse](500, "Internal Server Error")
	}

	var rsp []v1.FlowResponse
	for _, flow := range flows {
		rsp = append(rsp, protoToFlow(flow))
	}

	return handler.ResponseType[[]v1.FlowResponse]{Body: rsp}
}

func (hdlr *flowsHdlr) Stream(ctx context.Context, params v1.StreamFlowsParams, rspStream handler.ResponseStream[v1.FlowResponse]) handler.ResponseType[handler.NoBody] {
	logrus.Info("stream flows")
	
	flowStream, err := hdlr.gmCli.Stream(ctx, &proto.FlowRequest{})
	if err != nil {
		logrus.WithError(err).Error("failed to stream flows")
		return handler.NewErrorResponse[handler.NoBody](500, "Internal Server Error")
	}

	for {
		flow, err := flowStream.Recv()
		if err != nil {
			logrus.WithError(err).Error("failed to stream flows")
			break
		}

		if err := rspStream.Write(protoToFlow(flow)); err != nil {
			logrus.WithError(err).Error("failed to write flow response")
			return handler.NewErrorResponse[handler.NoBody](500, "Internal Server Error")
		}
	}

	return handler.ResponseType[handler.NoBody]{Status: http.StatusOK}
}

func protoToFlow(flow *proto.Flow) v1.FlowResponse {
	return v1.FlowResponse{
		StartTime: time.Unix(flow.StartTime, 0),
		EndTime:   time.Unix(flow.EndTime, 0),
		Action:    flow.Key.Action,

		SourceName:      flow.Key.SourceName,
		SourceNamespace: flow.Key.SourceNamespace,
		SourceLabels:    strings.Join(flow.SourceLabels, " | "),

		DestName:      flow.Key.DestName,
		DestNamespace: flow.Key.DestNamespace,
		DestLabels:    strings.Join(flow.DestLabels, " | "),

		Protocol:   flow.Key.Proto,
		DestPort:   flow.Key.DestPort,
		Reporter:   flow.Key.Reporter,
		PacketsIn:  flow.PacketsIn,
		PacketsOut: flow.PacketsOut,
		BytesIn:    flow.BytesIn,
		BytesOut:   flow.PacketsIn,
	}
}
