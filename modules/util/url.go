// Copyright 2019 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package util

import (
	"errors"
	"net/url"
	"path"
	"strings"
)

// PathEscapeSegments escapes segments of a path while not escaping forward slash
func PathEscapeSegments(path string) string {
	slice := strings.Split(path, "/")
	for index := range slice {
		slice[index] = url.PathEscape(slice[index])
	}
	escapedPath := strings.Join(slice, "/")
	return escapedPath
}

// URLJoin joins url components, like path.Join, but preserving contents
func URLJoin(base string, elems ...string) string {
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return ""
	}
	joinedPath := path.Join(elems...)
	argURL, err := url.Parse(joinedPath)
	if err != nil {
		return ""
	}
	joinedURL := baseURL.ResolveReference(argURL).String()
	if !baseURL.IsAbs() && !strings.HasPrefix(base, "/") {
		return joinedURL[1:] // Removing leading '/' if needed
	}
	return joinedURL
}

func SanitizeURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	u.User = nil
	return u.String(), nil
}

// NormalizeAbsoluteURL renders an absolute URL in a form that can be compared byte-for-byte:
// lower-case scheme and host, no default port, non-empty path. It deliberately does not touch the
// query - parameter order is meaningful to anything that signed over the URL.
//
// It lives here rather than next to its caller because two layers need the same answer: the
// NIP-98 verifier in services/agentauth compares a signed `u` tag against the server's own URL,
// and models/agent re-runs that comparison when an auditor asks whether a stored row still
// matches the signature it was written from. Two normalizers that drifted apart would make a
// truthful row look forged.
func NormalizeAbsoluteURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	if scheme == "" || host == "" {
		return "", errors.New("url is not absolute")
	}
	switch {
	case scheme == "http" && strings.HasSuffix(host, ":80"):
		host = strings.TrimSuffix(host, ":80")
	case scheme == "https" && strings.HasSuffix(host, ":443"):
		host = strings.TrimSuffix(host, ":443")
	}
	urlPath := u.EscapedPath()
	if urlPath == "" {
		urlPath = "/"
	}
	out := scheme + "://" + host + urlPath
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out, nil
}
