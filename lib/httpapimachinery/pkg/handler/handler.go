package handler

import (
	"context"
	"encoding/json"
	"github.com/sirupsen/logrus"
	"net/http"
	"strings"

	api "github.com/projectcalico/calico/lib/httpapimachinery/pkg/encoding"
)

// handler is an unexported http.Handler, used to force APIs to get the handler implementations from this package and
// implement missing handlers here. These handlers are responsible for reading the request, decoding them into concreate
// objects to pass to some "backend" handler, retrieves the response from the backend handlers and encodes the response
// properly. This abstracts out all http request / response handling logic from the backend implementation.
type handler http.Handler

func NewBasicJSONHandler[RequestParams any, Body any](f func(context.Context, RequestParams) ResponseType[Body]) handler {
	return genericJSONHandler[RequestParams, Body]{
		f: f,
	}
}

// genericJSONHandler is a handler that accepts either no body or a json body in the request and response with a json
// object. If the api needs to accept lists of objects or respond with them then this is not suitable, use something like
// ndJSONReqRespHandler or ndJSONRespHandler.
type genericJSONHandler[RequestParams any, Body any] struct {
	f func(context.Context, RequestParams) ResponseType[Body]
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

type ResponseStream[E any] interface {
	Write(E) error
}

type jsonStreamWriter[E any] struct {
	w http.Flusher
	e *json.Encoder
}

func newJSONStreamWriter[E any](w http.ResponseWriter) *jsonStreamWriter[E] {
	return &jsonStreamWriter[E]{w: w.(http.Flusher), e: json.NewEncoder(w)}
}

func (w *jsonStreamWriter[E]) Write(e E) error {
	if err := w.e.Encode(e); err != nil {
		return err
	}

	// TODO Add batching ability.
	w.w.Flush()
	return nil
}

func NewJSONStreamHandler[RequestParams any, Response any](f func(context.Context, RequestParams, ResponseStream[Response]) ResponseType[NoBody]) jsonStreamHandler[RequestParams, Response] {
	return jsonStreamHandler[RequestParams, Response]{f: f}
}

type jsonStreamHandler[RequestParams any, Response any] struct {
	f func(context.Context, RequestParams, ResponseStream[Response]) ResponseType[NoBody]
}

func (hdlr jsonStreamHandler[RequestParams, Response]) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	params, err := api.DecodeAndValidateReqParams[RequestParams](req)
	if err != nil {
		// TODO handle error.
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	jStream := newJSONStreamWriter[Response](w)

	response := hdlr.f(req.Context(), *params, jStream)
	if len(response.Errors) != 0 {
		writeJSONError(w, response.Status, strings.Join(response.Errors, "; "))
	}
}
