/*
 * Copyright (c) 2022, MegaEase
 * All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package http is the HTTP probe package.
package http

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"

	"github.com/megaease/easeprobe/eval"
	"github.com/megaease/easeprobe/global"
	"github.com/megaease/easeprobe/probe"
	"github.com/megaease/easeprobe/probe/base"
	"github.com/prometheus/client_golang/prometheus"

	log "github.com/sirupsen/logrus"
)

// HTTP implements a config for HTTP.
type HTTP struct {
	base.DefaultProbe `yaml:",inline"`
	URL               string            `yaml:"url"`
	Proxy             string            `yaml:"proxy"`
	ContentEncoding   string            `yaml:"content_encoding,omitempty"`
	Method            string            `yaml:"method,omitempty"`
	Headers           map[string]string `yaml:"headers,omitempty"`
	Body              string            `yaml:"body,omitempty"`

	// Output Text Checker
	probe.TextChecker `yaml:",inline"`

	// Evaluator
	Evaluator eval.Evaluator `yaml:"eval,omitempty"`

	// Option - HTTP Basic Auth Credentials
	User string `yaml:"username,omitempty"`
	Pass string `yaml:"password,omitempty"`

	// Option - Preferred HTTP response code ranges, only HTTP standard codes(smaller than 500) are supported;
	// If no set, default is [0, 499].
	SuccessCode [][]int `yaml:"success_code,omitempty"`

	// Option - TLS Config
	global.TLS `yaml:",inline"`

	client *http.Client `yaml:"-"`

	traceStats *TraceStats `yaml:"-"`

	metrics *metrics `yaml:"-"`
}

func checkHTTPMethod(m string) bool {
	methods := [...]string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE"}
	for _, method := range methods {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}

// Config HTTP Config Object
func (h *HTTP) Config(gConf global.ProbeSettings) error {
	kind := "http"
	tag := ""
	name := h.ProbeName
	h.DefaultProbe.Config(gConf, kind, tag, name, h.URL, h.DoProbe)

	if _, err := url.ParseRequestURI(h.URL); err != nil {
		log.Errorf("[%s / %s] URL is not valid - %+v url=%+v", h.ProbeKind, h.ProbeName, err, h.URL)
		return err
	}

	tls, err := h.TLS.Config()
	if err != nil {
		log.Errorf("[%s / %s] TLS configuration error - %s", h.ProbeKind, h.ProbeName, err)
		return err
	}

	// security check
	log.Debugf("[%s / %s] the security checks %s", h.ProbeKind, h.ProbeName, strconv.FormatBool(h.Insecure))

	// create http transport configuration
	transport := &http.Transport{
		TLSClientConfig:   tls,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: h.Timeout()}
			conn, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			tcpConn, ok := conn.(*net.TCPConn)
			if ok {
				log.Debugf("[%s / %s] dial %s:%s", h.ProbeKind, h.ProbeName, network, addr)
				tcpConn.SetLinger(0) // disable the default TCP delayed-close behavior,
				// which send the RST to the peer when the connection is closed.
				// this would remove the TIME_wAIT state of the TCP connection.
				return tcpConn, nil
			}
			return conn, nil
		},
		Proxy: http.ProxyFromEnvironment, // use proxy from environment variables
	}

	// proxy server
	if len(strings.TrimSpace(h.Proxy)) > 0 {
		proxyURL, err := url.Parse(h.Proxy)
		if err != nil {
			log.Errorf("[%s / %s] proxy URL is not valid - %+v", h.ProbeKind, h.ProbeName, err)
			return err
		}
		transport.Proxy = http.ProxyURL(proxyURL)
		log.Debugf("[%s / %s] proxy server is %s", h.ProbeKind, h.ProbeName, proxyURL)
	}

	h.client = &http.Client{
		Timeout:   h.Timeout(),
		Transport: transport,
	}

	if !checkHTTPMethod(h.Method) {
		h.Method = "GET"
	}

	var codeRange [][]int
	for _, r := range h.SuccessCode {
		if len(r) != 2 {
			log.Warnf("[%s/ %s] HTTP Success Code range is not valid - %v, skip", h.ProbeKind, h.ProbeName, r)
			continue
		}
		codeRange = append(codeRange, []int{r[0], r[1]})
	}
	if len(codeRange) == 0 {
		codeRange = [][]int{{0, 499}}
	}
	h.SuccessCode = codeRange

	if err := h.TextChecker.Config(); err != nil {
		return err
	}

	if err := h.Evaluator.Config(); err != nil {
		return err
	}

	h.metrics = newMetrics(kind, tag)

	log.Debugf("[%s / %s] configuration: %+v", h.ProbeKind, h.ProbeName, h)
	return nil
}

// DoProbe return the checking result
func (h *HTTP) DoProbe() (bool, string) {
	req, err := http.NewRequest(h.Method, h.URL, bytes.NewBuffer([]byte(h.Body)))
	if err != nil {
		return false, fmt.Sprintf("HTTP request error - %v", err)
	}
	if len(h.User) > 0 && len(h.Pass) > 0 {
		req.SetBasicAuth(h.User, h.Pass)
	}
	if len(h.ContentEncoding) > 0 {
		req.Header.Set("Content-Type", h.ContentEncoding)
	}
	req.Header.Set("User-Agent", global.OrgProgVer)
	for k, v := range h.Headers {
		req.Header.Set(k, v)
	}

	// client close the connection
	req.Close = true

	// Tracing HTTP request
	// set the http client trace
	h.traceStats = NewTraceStats(h.ProbeKind, "TRACE", h.ProbeName)
	clientTraceCtx := httptrace.WithClientTrace(req.Context(), h.traceStats.clientTrace)
	req = req.WithContext(clientTraceCtx)

	resp, err := h.client.Do(req)
	h.traceStats.Done()

	h.ExportMetrics(resp)
	if err != nil {
		log.Errorf("[%s / %s] error making get request: %v", h.ProbeKind, h.ProbeName, err)
		return false, fmt.Sprintf("Error: %v", err)
	}
	// Read the response body
	defer resp.Body.Close()
	response, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Debugf("%s", string(response))
		return false, fmt.Sprintf("Error: %v", err)
	}

	var valid bool
	for _, r := range h.SuccessCode {
		if r[0] <= resp.StatusCode && resp.StatusCode <= r[1] {
			valid = true
			break
		}
	}
	if !valid {
		return false, fmt.Sprintf("HTTP Status Code is %d. It missed in %v", resp.StatusCode, h.SuccessCode)
	}

	message := fmt.Sprintf("HTTP Status Code is %d", resp.StatusCode)

	log.Debugf("[%s / %s] - %s", h.ProbeKind, h.ProbeName, h.TextChecker.String())
	if err := h.Check(string(response)); err != nil {
		log.Errorf("[%s / %s] - %v", h.ProbeKind, h.ProbeName, err)
		message += fmt.Sprintf(". Error: %v", err)
		return false, message
	}

	if h.Evaluator.DocType != eval.Unsupported && h.Evaluator.Extractor != nil &&
		len(strings.TrimSpace(h.Evaluator.Expression)) > 0 {

		log.Debugf("[%s / %s] - Evaluator expression: %s", h.ProbeKind, h.ProbeName, h.Evaluator.Expression)
		h.Evaluator.SetDocument(h.Evaluator.DocType, string(response))
		result, err := h.Evaluator.Evaluate()
		if err != nil {
			log.Errorf("[%s / %s] - %v", h.ProbeKind, h.ProbeName, err)
			message += fmt.Sprintf(". Evaluation Error: %v", err)
			return false, message
		}
		if !result {
			log.Errorf("[%s / %s] - expression is evaluated to false!", h.ProbeKind, h.ProbeName)
			message += fmt.Sprintf(". Expression is evaluated to false!")
			return false, message
		}
		log.Debugf("[%s / %s] - expression is evaluated to true!", h.ProbeKind, h.ProbeName)
	}

	return true, message
}

// ExportMetrics export HTTP metrics
func (h *HTTP) ExportMetrics(resp *http.Response) {
	code := 0 // no response
	len := 0
	if resp != nil {
		code = resp.StatusCode
		len = int(resp.ContentLength)
	}
	h.metrics.StatusCode.With(prometheus.Labels{
		"name":   h.ProbeName,
		"status": fmt.Sprintf("%d", code),
	}).Inc()

	h.metrics.ContentLen.With(prometheus.Labels{
		"name":   h.ProbeName,
		"status": fmt.Sprintf("%d", code),
	}).Set(float64(len))

	h.metrics.DNSDuration.With(prometheus.Labels{
		"name":   h.ProbeName,
		"status": fmt.Sprintf("%d", code),
	}).Set(toMS(h.traceStats.dnsTook))

	h.metrics.ConnectDuration.With(prometheus.Labels{
		"name":   h.ProbeName,
		"status": fmt.Sprintf("%d", code),
	}).Set(toMS(h.traceStats.connTook))

	h.metrics.TLSDuration.With(prometheus.Labels{
		"name":   h.ProbeName,
		"status": fmt.Sprintf("%d", code),
	}).Set(toMS(h.traceStats.tlsTook))

	h.metrics.SendDuration.With(prometheus.Labels{
		"name":   h.ProbeName,
		"status": fmt.Sprintf("%d", code),
	}).Set(toMS(h.traceStats.sendTook))

	h.metrics.WaitDuration.With(prometheus.Labels{
		"name":   h.ProbeName,
		"status": fmt.Sprintf("%d", code),
	}).Set(toMS(h.traceStats.waitTook))

	h.metrics.TransferDuration.With(prometheus.Labels{
		"name":   h.ProbeName,
		"status": fmt.Sprintf("%d", code),
	}).Set(toMS(h.traceStats.transferTook))

	h.metrics.TotalDuration.With(prometheus.Labels{
		"name":   h.ProbeName,
		"status": fmt.Sprintf("%d", code),
	}).Set(toMS(h.traceStats.totalTook))
}
