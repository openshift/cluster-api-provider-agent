/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"net"
	"net/url"
	"strings"
)

// normalizeHTTPURL rewrites HTTP(S) URLs that contain an unbracketed IPv6 literal host
// so they can be parsed by net/url (Go 1.26+).
func normalizeHTTPURL(rawURL string) string {
	if rawURL == "" {
		return rawURL
	}

	// Already a valid URL; no rewriting needed.
	if _, err := url.Parse(rawURL); err == nil {
		return rawURL
	}

	const schemeSep = "://"
	schemeIdx := strings.Index(rawURL, schemeSep)
	if schemeIdx < 0 {
		return rawURL
	}
	scheme := rawURL[:schemeIdx+len(schemeSep)]
	remainder := rawURL[schemeIdx+len(schemeSep):]

	path := ""
	if slash := strings.Index(remainder, "/"); slash >= 0 {
		path = remainder[slash:]
		remainder = remainder[:slash]
	}

	// Host is already RFC 3986 bracket form; leave unchanged.
	if strings.HasPrefix(remainder, "[") {
		return rawURL
	}

	// Split host from port on the last colon (IPv6 literals contain multiple colons).
	lastColon := strings.LastIndex(remainder, ":")
	if lastColon < 0 {
		return rawURL
	}

	host := remainder[:lastColon]
	port := remainder[lastColon+1:]
	// Only rewrite when host is an IPv6 literal and a port is present.
	if port == "" || net.ParseIP(host) == nil || !strings.Contains(host, ":") {
		return rawURL
	}

	return scheme + net.JoinHostPort(host, port) + path
}

// ignitionEndpointURLForACI normalizes the ignition endpoint URL and strips a trailing path
// segment (for example /ignition) before persisting it on AgentClusterInstall.
func ignitionEndpointURLForACI(raw string) string {
	normalized := normalizeHTTPURL(raw)

	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme == "" {
		return normalized
	}

	parsed.Path = ""
	parsed.RawPath = ""
	return parsed.String()
}
