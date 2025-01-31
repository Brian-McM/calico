package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/projectcalico/calico/lib/httpapimachinery/pkg/handler"
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/gomega"
)

func TestIngestFlowLogs(t *testing.T) {
	RegisterTestingT(t)
	type Request struct{}
	hdlr := handler.NewJSONStreamHandler(func(ctx context.Context, params Request, stream handler.ResponseStream[map[string]string]) handler.ResponseType[handler.NoBody] {
		if err := stream.Write(map[string]string{
			"foo": "bar",
		}); err != nil {
			return handler.NewErrorResponse[handler.NoBody](500, "bad stuff")
		}

		if err := stream.Write(map[string]string{
			"baz": "bar",
		}); err != nil {
			return handler.NewErrorResponse[handler.NoBody](500, "bad stuff")
		}

		return handler.NewResponse[handler.NoBody]()
	})

	w := httptest.NewRecorder()
	body, err := json.Marshal(Request{})
	Expect(err).NotTo(HaveOccurred())
	r, err := http.NewRequest(http.MethodGet, "foobar", bytes.NewBuffer(body))
	Expect(err).NotTo(HaveOccurred())

	hdlr.ServeHTTP(w, r)

	Expect(w.Code).To(Equal(http.StatusOK))
	Expect(w.Body.String()).To(Equal("{\"foo\":\"bar\"}\n{\"baz\":\"bar\"}\n"))
}
