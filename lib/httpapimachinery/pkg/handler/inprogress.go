package handler

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"

	api "github.com/projectcalico/calico/lib/httpapimachinery/pkg/encoding"
)

// ndJSONReqRespHandler accepts a json object or an empty body request and responds with a ndjson list of objects.
type ndJSONRespHandler[RequestParams any, Body any] struct {
	f func(context.Context, RequestParams) ResponseType[iter.Seq[Body]]
}

func NewNDJSONRespHandler[RequestParams any, Body any](f func(context.Context, RequestParams) ResponseType[iter.Seq[Body]]) handler {
	return ndJSONRespHandler[RequestParams, Body]{
		f: f,
	}
}

func (g ndJSONRespHandler[RequestParams, Response]) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	params, err := api.DecodeAndValidateReqParams[RequestParams](req)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	rsp := g.f(req.Context(), *params)
	if rsp.Errors != nil {
		writeJSONError(w, rsp.Status, strings.Join(rsp.Errors, "; "))
		return
	} else {
		writeNDJSONResponse(w, rsp.Body)
	}
}

func writeNDJSONResponse[E any](w http.ResponseWriter, src iter.Seq[E]) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	jEncoder := json.NewEncoder(w)
	for response := range src {
		if err := jEncoder.Encode(response); err != nil {
			logrus.WithError(err).Error("Failed to encode response.")
		}
	}
}
