// Copyright 2020 The Go Authors. All rights reserved.
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file or at
// https://developers.google.com/open-source/licenses/bsd.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// makePkgGoDevRequest makes a request to the teeproxy with data about the
// godoc.org request.
func makePkgGoDevRequest(r *http.Request, latency time.Duration, isRobot bool, status int) {
	var msg string
	defer func() {
		log.Printf("makePkgGoDevRequest(%q): %s", r.URL.Path, msg)
	}()

	if !shouldTeeRequest(r.URL.Path) {
		msg = "not teeing request"
		return
	}
	event := newGDDOEvent(r, latency, isRobot, status)
	b, err := json.Marshal(event)
	if err != nil {
		msg = fmt.Sprintf("json.Marshal(%v): %v", event, err)
		return
	}

	teeproxyURL := url.URL{Scheme: "https", Host: teeproxyHost}
	if _, err := http.Post(teeproxyURL.String(), jsonMIMEType, bytes.NewReader(b)); err != nil {
		msg = fmt.Sprintf("http.Post(%q, %q, %v): %v", teeproxyURL.String(), jsonMIMEType, event, err)
		return
	}
	msg = fmt.Sprintf("request made to %q for %+v", teeproxyURL.String(), event)
}

// doNotTeeURLsToPkgGoDev are paths that should not be teed to pkg.go.dev.
var doNotTeeURLsToPkgGoDev = map[string]bool{
	"/-/bot":     true,
	"/-/refresh": true,
}

// doNotTeeExtsToPkgGoDev are URL extensions that should not be teed to
// pkg.go.dev.
var doNotTeeExtsToPkgGoDev = map[string]bool{
	".css":  true,
	".html": true,
	".js":   true,
	".txt":  true,
	".xml":  true,
}

// shouldTeeRequest reports whether a request should be teed to pkg.go.dev.
func shouldTeeRequest(u string) bool {
	// Don't tee App Engine requests to pkg.go.dev.
	if strings.HasPrefix(u, "/_ah/") {
		return false
	}
	ext := filepath.Ext(u)
	if doNotTeeExtsToPkgGoDev[ext] {
		return false
	}
	if doNotTeeURLsToPkgGoDev[u] {
		return false
	}
	return true
}

type gddoEvent struct {
	Host        string
	Path        string
	Status      int
	URL         string
	Header      http.Header
	Latency     time.Duration
	IsRobot     bool
	UsePkgGoDev bool
}

func newGDDOEvent(r *http.Request, latency time.Duration, isRobot bool, status int) *gddoEvent {
	targetURL := url.URL{
		Scheme:   "https",
		Host:     r.URL.Host,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}
	if targetURL.Host == "" && r.Host != "" {
		targetURL.Host = r.Host
	}
	return &gddoEvent{
		Host:        targetURL.Host,
		Path:        r.URL.Path,
		Status:      status,
		URL:         targetURL.String(),
		Header:      r.Header,
		Latency:     latency,
		IsRobot:     isRobot,
		UsePkgGoDev: shouldRedirectToPkgGoDev(r),
	}
}

func userReturningFromPkgGoDev(req *http.Request) bool {
	return req.FormValue("utm_source") == "backtogodoc"
}

const (
	pkgGoDevRedirectCookie = "pkggodev-redirect"
	pkgGoDevRedirectParam  = "redirect"
	pkgGoDevRedirectOn     = "on"
	pkgGoDevRedirectOff    = "off"
	pkgGoDevHost           = "pkg.go.dev"
	teeproxyHost           = "teeproxy-dot-go-discovery.appspot.com"
)

func shouldRedirectToPkgGoDev(req *http.Request) bool {
	// API requests are not redirected.
	if strings.HasPrefix(req.URL.Host, "api") {
		return false
	}
	redirectParam := req.FormValue(pkgGoDevRedirectParam)
	if redirectParam == pkgGoDevRedirectOn || redirectParam == pkgGoDevRedirectOff {
		return redirectParam == pkgGoDevRedirectOn
	}
	cookie, err := req.Cookie(pkgGoDevRedirectCookie)
	return (err == nil && cookie.Value == pkgGoDevRedirectOn)
}

// pkgGoDevRedirectHandler redirects requests from godoc.org to pkg.go.dev,
// based on whether a cookie is set for pkggodev-redirect. The cookie
// can be turned on/off using a query param.
func pkgGoDevRedirectHandler(f func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		if userReturningFromPkgGoDev(r) {
			return f(w, r)
		}

		redirectParam := r.FormValue(pkgGoDevRedirectParam)

		if redirectParam == pkgGoDevRedirectOn {
			cookie := &http.Cookie{Name: pkgGoDevRedirectCookie, Value: redirectParam, Path: "/"}
			http.SetCookie(w, cookie)
		}
		if redirectParam == pkgGoDevRedirectOff {
			cookie := &http.Cookie{Name: pkgGoDevRedirectCookie, Value: "", MaxAge: -1, Path: "/"}
			http.SetCookie(w, cookie)
		}

		if !shouldRedirectToPkgGoDev(r) {
			return f(w, r)
		}

		http.Redirect(w, r, pkgGoDevURL(r.URL).String(), http.StatusFound)
		return nil
	}
}

func pkgGoDevURL(godocURL *url.URL) *url.URL {
	u := &url.URL{Scheme: "https", Host: pkgGoDevHost}
	q := url.Values{"utm_source": []string{"godoc"}}

	if strings.Contains(godocURL.Path, "/vendor/") || strings.HasSuffix(godocURL.Path, "/vendor") {
		u.Path = "/"
		u.RawQuery = q.Encode()
		return u
	}

	switch godocURL.Path {
	case "/-/go":
		u.Path = "/std"
		q.Add("tab", "packages")
	case "/-/about":
		u.Path = "/about"
	case "/":
		if qparam := godocURL.Query().Get("q"); qparam != "" {
			u.Path = "/search"
			q.Set("q", qparam)
		} else {
			u.Path = "/"
		}
	case "/-/subrepo":
		u.Path = "/search"
		q.Set("q", "golang.org/x")
	default:
		{
			u.Path = godocURL.Path
			if _, ok := godocURL.Query()["imports"]; ok {
				q.Set("tab", "imports")
			} else if _, ok := godocURL.Query()["importers"]; ok {
				q.Set("tab", "importedby")
			} else {
				q.Set("tab", "doc")
			}
		}
	}

	u.RawQuery = q.Encode()
	return u
}
