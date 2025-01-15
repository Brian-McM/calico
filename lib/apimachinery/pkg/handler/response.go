package handler

// NoBody is a type to use when a handler doesn't respond with a body. The avoids having two ResponseTypes, one for
// responses with a body and one without.
// TODO Revisit if we should just have two response types??
type NoBody int

// ResponseType is an object that APIs return for the generic handlers to respond with. It's up to the generic handlers
// to decide how they handle errors and the status as well as format the Body returned (i.e. JSON, CSV, other...).
// This helps abstract http response logic / formatting from the API implementations.
type ResponseType[E any] struct {
	// Headers are the addition headers to response with.
	// TODO do we really need to explicitly send these back? Maybe we can do something with the object returned to infer
	// TODO whatever headers we need, possibly by inferring the type??
	Headers map[string]string
	Status  int
	Errors  []string
	Body    E
}

func NewResponse[E any]() ResponseType[E] {
	return ResponseType[E]{
		Headers: make(map[string]string),
		Status:  200,
	}
}

func (rsp ResponseType[E]) SetStatus(status int) ResponseType[E] {
	rsp.Status = status
	return rsp
}

func (rsp ResponseType[E]) AddHeader(name, value string) ResponseType[E] {
	rsp.Headers[name] = value
	return rsp
}

func NewErrorResponse[E any](code int, messages ...string) ResponseType[E] {
	return ResponseType[E]{
		Headers: make(map[string]string),
		Status:  code,
		Errors:  messages,
	}
}
