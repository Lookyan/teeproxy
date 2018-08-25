package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"runtime"
	"strings"
	"time"
)

// Console flags
var (
	listen                = flag.String("l", ":8888", "port to accept requests")
	targetProduction      = flag.String("a", "localhost:8080", "where production traffic goes. http://localhost:8080/production")
	altTarget             = flag.String("b", "localhost:8081", "where testing traffic goes. response are skipped. http://localhost:8081/test")
	debug                 = flag.Bool("debug", false, "more logging, showing ignored output")
	productionTimeout     = flag.Int("a.timeout", 2500, "timeout in milliseconds for production traffic")
	alternateTimeout      = flag.Int("b.timeout", 1000, "timeout in milliseconds for alternate site traffic")
	productionHostRewrite = flag.Bool("a.rewrite", false, "rewrite the host header when proxying production traffic")
	alternateHostRewrite  = flag.Bool("b.rewrite", false, "rewrite the host header when proxying alternate site traffic")
	percent               = flag.Float64("p", 100.0, "float64 percentage of traffic to send to testing")
	tlsPrivateKey         = flag.String("key.file", "", "path to the TLS private key file")
	tlsCertificate        = flag.String("cert.file", "", "path to the TLS certificate file")
	forwardClientIP       = flag.Bool("forward-client-ip", false, "enable forwarding of the client IP to the backend using the 'X-Forwarded-For' and 'Forwarded' headers")
	closeConnections      = flag.Bool("close-connections", false, "close connections to the clients and backends")
)

// Sets the request URL.
//
// This turns a inbound request (a request without URL) into an outbound request.
func setRequestTarget(request *http.Request, target *string) {
	URL, err := url.Parse("http://" + *target + request.URL.String())
	if err != nil {
		log.Println(err)
	}
	request.URL = URL
}

// Sends a request and returns the response.
func handleRequest(request *http.Request, timeout time.Duration) *http.Response {
	transport := &http.Transport{
		// NOTE(girone): DialTLS is not needed here, because the teeproxy works
		// as an SSL terminator.
		Dial: (&net.Dialer{ // go1.8 deprecated: Use DialContext instead
			Timeout:   timeout,
			KeepAlive: 10 * timeout,
		}).Dial,
		// Close connections to the production and alternative servers?
		DisableKeepAlives: *closeConnections,
		//IdleConnTimeout: timeout,  // go1.8
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: timeout,
	}
	// Do not use http.Client here, because it's higher level and processes
	// redirects internally, which is not what we want.
	//client := &http.Client{
	//	Timeout: timeout,
	//	Transport: transport,
	//}
	//response, err := client.Do(request)
	response, err := transport.RoundTrip(request)
	if err != nil {
		log.Println("Request failed:", err)
	}
	return response
}

// Sends a request and returns channel to wait for response.
func handleAsyncRequest(request *http.Request, timeout time.Duration) chan *http.Response {
	ch := make(chan *http.Response)
	transport := &http.Transport{
		// NOTE(girone): DialTLS is not needed here, because the teeproxy works
		// as an SSL terminator.
		Dial: (&net.Dialer{ // go1.8 deprecated: Use DialContext instead
			Timeout:   timeout,
			KeepAlive: 10 * timeout,
		}).Dial,
		// Close connections to the production and alternative servers?
		DisableKeepAlives: *closeConnections,
		//IdleConnTimeout: timeout,  // go1.8
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: timeout,
	}
	go func() {
		response, err := transport.RoundTrip(request)
		if err != nil {
			log.Println("Request failed:", err)
		}
		ch <- response
	}()
	return ch
}

// process response. Return true if resp is not nil
func processResponse(resp *http.Response, w http.ResponseWriter) []byte {
	if resp != nil {
		defer resp.Body.Close()

		// Forward response headers.
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)

		// Forward response body.
		body, _ := ioutil.ReadAll(resp.Body)
		w.Write(body)
		return body
	}
	return nil
}

// compareResp compares responses assuming there is a json inside of body
func compareResp(respProdBody []byte, respAlt *http.Response) {
	if respAlt == nil {
		// TODO: log alternative request error
	} else {
		defer respAlt.Body.Close()

		// don't compare headers

		// Get entire response body.
		respAltBody, _ := ioutil.ReadAll(respAlt.Body)
		var respProdDeserealized interface{}
		var respAltDeserealized interface{}
		err := json.Unmarshal(respProdBody, respProdDeserealized)
		if err != nil {
			// then compare bytes
			if bytes.Equal(respProdBody, respAltBody) {
				log.Println("Equal")
				return
			} else {
				log.Println("Not equal")
				return
			}
		}
		err = json.Unmarshal(respAltBody, respAltDeserealized)
		if err != nil {
			if bytes.Equal(respProdBody, respAltBody) {
				log.Println("Equal")
				return
			} else {
				log.Println("Not equal")
				return
			}
		}

		if respAltDeserealized != respProdDeserealized {
			log.Println("Not equal")
		} else {
			log.Println("Equal")
		}

	}
}

// handler contains the address of the main Target and the one for the Alternative target
type handler struct {
	Target      string
	Alternative string
	Randomizer  rand.Rand
}

// ServeHTTP duplicates the incoming request (req) and does the request to the
// Target and the Alternate target discading the Alternate response
func (h handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var productionRequest, alternativeRequest *http.Request
	if *forwardClientIP {
		updateForwardedHeaders(req)
	}

	// preparing prod request (we always need it)
	alternativeRequest, productionRequest = DuplicateRequest(req)
	setRequestTarget(productionRequest, targetProduction)
	if *productionHostRewrite {
		productionRequest.Host = h.Target
	}
	timeoutProd := time.Duration(*productionTimeout) * time.Millisecond

	defer func() {
		if r := recover(); r != nil && *debug {
			log.Println("Recovered in ServeHTTP from:", r)
		}
	}()

	if *percent == 100.0 || h.Randomizer.Float64()*100 < *percent {

		setRequestTarget(alternativeRequest, altTarget)
		if *alternateHostRewrite {
			alternativeRequest.Host = h.Alternative
		}
		timeoutAlt := time.Duration(*alternateTimeout) * time.Millisecond

		prodRespCh := handleAsyncRequest(productionRequest, timeoutProd)
		altRespCh := handleAsyncRequest(alternativeRequest, timeoutAlt)

		select {
		case prodResp := <-prodRespCh:
			respProdBody := processResponse(prodResp, w)
			if respProdBody != nil {
				go func() {
					altResp := <-altRespCh
					compareResp(respProdBody, altResp)
				}()
			}
		case altResp := <-altRespCh:
			prodResp := <-prodRespCh
			respProdBody := processResponse(prodResp, w)
			go compareResp(respProdBody, altResp)
		}

		return

	} else {
		productionRequest = req
	}

	alternativeRequest = nil
	respCh := handleAsyncRequest(productionRequest, timeoutProd)

	resp := <-respCh

	processResponse(resp, w)
}

func main() {
	flag.Parse()

	log.Printf("Starting teeproxy at %s sending to A: %s and B: %s",
		*listen, *targetProduction, *altTarget)

	runtime.GOMAXPROCS(runtime.NumCPU())

	var err error

	var listener net.Listener

	if len(*tlsPrivateKey) > 0 {
		cer, err := tls.LoadX509KeyPair(*tlsCertificate, *tlsPrivateKey)
		if err != nil {
			log.Fatalf("Failed to load certficate: %s and private key: %s", *tlsCertificate, *tlsPrivateKey)
		}

		config := &tls.Config{Certificates: []tls.Certificate{cer}}
		listener, err = tls.Listen("tcp", *listen, config)
		if err != nil {
			log.Fatalf("Failed to listen to %s: %s", *listen, err)
		}
	} else {
		listener, err = net.Listen("tcp", *listen)
		if err != nil {
			log.Fatalf("Failed to listen to %s: %s", *listen, err)
		}
	}

	h := handler{
		Target:      *targetProduction,
		Alternative: *altTarget,
		Randomizer:  *rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	server := &http.Server{
		Handler: h,
	}
	if *closeConnections {
		// Close connections to clients by setting the "Connection": "close" header in the response.
		server.SetKeepAlivesEnabled(false)
	}
	server.Serve(listener)
}

type nopCloser struct {
	io.Reader
}

func (nopCloser) Close() error { return nil }

func DuplicateRequest(request *http.Request) (request1 *http.Request, request2 *http.Request) {
	b1 := new(bytes.Buffer)
	b2 := new(bytes.Buffer)
	w := io.MultiWriter(b1, b2)
	io.Copy(w, request.Body)
	defer request.Body.Close()
	request1 = &http.Request{
		Method:        request.Method,
		URL:           request.URL,
		Proto:         request.Proto,
		ProtoMajor:    request.ProtoMajor,
		ProtoMinor:    request.ProtoMinor,
		Header:        request.Header,
		Body:          nopCloser{b1},
		Host:          request.Host,
		ContentLength: request.ContentLength,
		Close:         true,
	}
	request2 = &http.Request{
		Method:        request.Method,
		URL:           request.URL,
		Proto:         request.Proto,
		ProtoMajor:    request.ProtoMajor,
		ProtoMinor:    request.ProtoMinor,
		Header:        request.Header,
		Body:          nopCloser{b2},
		Host:          request.Host,
		ContentLength: request.ContentLength,
		Close:         true,
	}
	return
}

func updateForwardedHeaders(request *http.Request) {
	positionOfColon := strings.LastIndex(request.RemoteAddr, ":")
	var remoteIP string
	if positionOfColon != -1 {
		remoteIP = request.RemoteAddr[:positionOfColon]
	} else {
		log.Printf("The default format of request.RemoteAddr should be IP:Port but was %s\n", remoteIP)
		remoteIP = request.RemoteAddr
	}
	insertOrExtendForwardedHeader(request, remoteIP)
	insertOrExtendXFFHeader(request, remoteIP)
}

const XFF_HEADER = "X-Forwarded-For"

func insertOrExtendXFFHeader(request *http.Request, remoteIP string) {
	header := request.Header.Get(XFF_HEADER)
	if header != "" {
		// extend
		request.Header.Set(XFF_HEADER, header+", "+remoteIP)
	} else {
		// insert
		request.Header.Set(XFF_HEADER, remoteIP)
	}
}

const FORWARDED_HEADER = "Forwarded"

// Implementation according to rfc7239
func insertOrExtendForwardedHeader(request *http.Request, remoteIP string) {
	extension := "for=" + remoteIP
	header := request.Header.Get(FORWARDED_HEADER)
	if header != "" {
		// extend
		request.Header.Set(FORWARDED_HEADER, header+", "+extension)
	} else {
		// insert
		request.Header.Set(FORWARDED_HEADER, extension)
	}
}
