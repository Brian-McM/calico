package handler

import (
	"context"
	"encoding/json"
	"github.com/sirupsen/logrus"
	"iter"
	"net/http"
	"strings"

	api "github.com/projectcalico/calico/lib/apimachinery/pkg/encoding"
)

// handler is an unexported http.Handler, used to force APIs to get the handler implementations from this package and
// implement missing handlers here. These handlers are responsible for reading the request, decoding them into concreate
// objects to pass to some "backend" handler, retrieves the response from the backend handlers and encodes the response
// properly. This abstracts out all http request / response handling logic from the backend implementation.
type handler http.Handler

// genericJSONHandler is a handler that accepts either no body or a json body in the request and response with a json
// object. If the api needs to accept lists of objects or respond with them then this is not suitable, use something like
// ndJSONReqRespHandler or ndJSONRespHandler.
type genericJSONHandler[RequestParams any, Body any] struct {
	f func(context.Context, RequestParams) ResponseType[Body]
}

func NewBasicJSONHandler[RequestParams any, Body any](f func(context.Context, RequestParams) ResponseType[Body]) handler {
	return genericJSONHandler[RequestParams, Body]{
		f: f,
	}
}

func (g genericJSONHandler[RequestParams, Response]) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	params, err := api.DecodeAndValidateReqParams[RequestParams](req)
	if err != nil {
		// TODO handle error.
		return
	}

	response := g.f(req.Context(), *params)
	if len(response.Errors) != 0 {
		writeJSONError(w, response.Status, strings.Join(response.Errors, "; "))
	} else {
		w.WriteHeader(200)
		writeJSONResponse(w, response.Body)
	}
}

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

func writeJSONResponse(w http.ResponseWriter, src any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(src); err != nil {
		logrus.WithError(err).Error("Failed to encode response.")
	}
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	writeJSONResponse(w, map[string]any{"errors": message})
}
