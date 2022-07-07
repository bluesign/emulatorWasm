//go:build js && wasm
// +build js,wasm

package wasmhttp

import (
	"bytes"
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"syscall/js"
)

// NewPromise creates a new JavaScript Promise
func NewPromise() (p js.Value, resolve func(interface{}), reject func(interface{})) {
	var cbFunc js.Func
	cbFunc = js.FuncOf(func(_ js.Value, args []js.Value) interface{} {
		cbFunc.Release()

		resolve = func(value interface{}) {
			args[0].Invoke(value)
		}

		reject = func(value interface{}) {
			args[1].Invoke(value)
		}

		return js.Undefined()
	})

	p = js.Global().Get("Promise").New(cbFunc)

	return
}

// Await waits for the Promise to be resolved and returns the value
func Await(p js.Value) (js.Value, error) {
	resCh := make(chan js.Value)
	var then js.Func
	then = js.FuncOf(func(_ js.Value, args []js.Value) interface{} {
		resCh <- args[0]
		return nil
	})
	defer then.Release()

	errCh := make(chan error)
	var catch js.Func
	catch = js.FuncOf(func(_ js.Value, args []js.Value) interface{} {
		errCh <- js.Error{args[0]}
		return nil
	})
	defer catch.Release()

	p.Call("then", then).Call("catch", catch)

	select {
	case res := <-resCh:
		return res, nil
	case err := <-errCh:
		return js.Undefined(), err
	}
}

// ResponseRecorder extends httptest.ResponseRecorder and implements js.Wrapper
type ResponseRecorder struct {
	*httptest.ResponseRecorder
}

// NewResponseRecorder returns a new ResponseRecorder
func NewResponseRecorder() ResponseRecorder {
	return ResponseRecorder{httptest.NewRecorder()}
}

//var _ js.Wrapper = ResponseRecorder{}

// JSValue builds and returns the equivalent JS Response (implementing js.Wrapper)
func (rr ResponseRecorder) JSValue() js.Value {
	var res = rr.Result()

	var body js.Value = js.Undefined()
	if res.ContentLength != 0 {
		var b, err = ioutil.ReadAll(res.Body)
		if err != nil {
			panic(err)
		}
		body = js.Global().Get("Uint8Array").New(len(b))
		js.CopyBytesToJS(body, b)
	}

	var init = make(map[string]interface{}, 2)

	if res.StatusCode != 0 {
		init["status"] = res.StatusCode
	}

	if len(res.Header) != 0 {
		var headers = make(map[string]interface{}, len(res.Header))
		for k := range res.Header {
			headers[k] = res.Header.Get(k)
		}
		init["headers"] = headers
	}

	return js.Global().Get("Response").New(body, init)
}
func Set(name string, value string) {
	Await(js.Global().Call("storage_set", js.ValueOf(name), js.ValueOf(value)))
}

func Get(name string) string {
	v, _ := Await(js.Global().Call("storage_get", js.ValueOf(name)))
	return v.String()
}

func Clear() {
	js.Global().Call("storage_clear")
}

func Log(entry *log.Entry) {
	j, _ := json.Marshal(entry.Data)
	js.Global().Call("emulator_log", js.ValueOf(entry.Message), js.ValueOf(string(j)), js.ValueOf(entry.Time.Format("15:04:05")))
}

func Request(r js.Value) *http.Request {
	jsBody := r.Get("body").String()
	req := httptest.NewRequest(
		r.Get("method").String(),
		r.Get("url").String(),
		bytes.NewBuffer([]byte(jsBody)),
	)
	return req
}

// Serve serves HTTP requests using handler or http.DefaultServeMux if handler is nil.
func Serve(handler http.Handler) func() {
	var h = handler
	if h == nil {
		h = http.DefaultServeMux
	}

	var cb = js.FuncOf(func(_ js.Value, args []js.Value) interface{} {
		var resPromise, resolve, reject = NewPromise()

		go func() {
			defer func() {
				if r := recover(); r != nil {
					if err, ok := r.(error); ok {
						reject(fmt.Sprintf("wasmhttp: panic: %+v\n", err))
					} else {
						reject(fmt.Sprintf("wasmhttp: panic: %v\n", r))
					}
				}
			}()

			var res = NewResponseRecorder()

			h.ServeHTTP(res, Request(args[0]))

			resolve(res.JSValue())
		}()

		return resPromise
	})

	js.Global().Call("setHandler", cb)

	return cb.Release
}
