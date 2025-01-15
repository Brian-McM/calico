package v1

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/goldmane/proto"
	"github.com/projectcalico/calico/lib/apimachinery/pkg/handler"
	v1 "github.com/projectcalico/calico/ui-api/pkg/apis/v1"
)

type flows struct {
	gCli proto.FlowAPIClient
}

func NewFlows(cli proto.FlowAPIClient) *flows {
	return &flows{cli}
}

func (r *flows) APIs() []handler.API {
	return []handler.API{
		{
			Method:  http.MethodGet,
			URL:     v1.ResourcesPath,
			Handler: handler.NewBasicJSONHandler(r.List),
		},
	}
}

func (r *flows) List(ctx context.Context, params v1.ListFlowsParams) handler.ResponseType[[]v1.FlowResponse] {
	flowStream, err := r.gCli.List(ctx, &proto.FlowRequest{})
	if err != nil {
		logrus.WithError(err).Error("failed to list flows")
		return handler.NewErrorResponse[[]v1.FlowResponse](500, "Internal Server Error")
	}

	var rsp []v1.FlowResponse
	for {
		flow, err := flowStream.Recv()
		if err != nil {
			break
		}
		rsp = append(rsp, v1.FlowResponse{
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
		})
	}

	return handler.ResponseType[[]v1.FlowResponse]{Body: rsp}
}
