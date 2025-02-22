/*
Copyright 2015 The Kubernetes Authors.

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

package template

import (
	"io/ioutil"
	"net"
	"os"
	"path"
	"reflect"
	"strings"
	"testing"

	"encoding/base64"
	"fmt"

	jsoniter "github.com/json-iterator/go"
	networking "k8s.io/api/networking/v1beta1"
	"k8s.io/ingress-nginx/internal/file"
	"k8s.io/ingress-nginx/internal/ingress"
	"k8s.io/ingress-nginx/internal/ingress/annotations/authreq"
	"k8s.io/ingress-nginx/internal/ingress/annotations/influxdb"
	"k8s.io/ingress-nginx/internal/ingress/annotations/luarestywaf"
	"k8s.io/ingress-nginx/internal/ingress/annotations/modsecurity"
	"k8s.io/ingress-nginx/internal/ingress/annotations/ratelimit"
	"k8s.io/ingress-nginx/internal/ingress/annotations/rewrite"
	"k8s.io/ingress-nginx/internal/ingress/controller/config"
)

var (
	// TODO: add tests for SSLPassthrough
	tmplFuncTestcases = map[string]struct {
		Path             string
		Target           string
		Location         string
		ProxyPass        string
		Sticky           bool
		XForwardedPrefix string
		SecureBackend    bool
		enforceRegex     bool
	}{
		"when secure backend enabled": {
			"/",
			"/",
			"/",
			"proxy_pass https://upstream_balancer;",
			false,
			"",
			true,
			false,
		},
		"when secure backend and dynamic config enabled": {
			"/",
			"/",
			"/",
			"proxy_pass https://upstream_balancer;",
			false,
			"",
			true,
			false,
		},
		"when secure backend, stickeness and dynamic config enabled": {
			"/",
			"/",
			"/",
			"proxy_pass https://upstream_balancer;",
			true,
			"",
			true,
			false,
		},
		"invalid redirect / to / with dynamic config enabled": {
			"/",
			"/",
			"/",
			"proxy_pass http://upstream_balancer;",
			false,
			"",
			false,
			false,
		},
		"invalid redirect / to /": {
			"/",
			"/",
			"/",
			"proxy_pass http://upstream_balancer;",
			false,
			"",
			false,
			false,
		},
		"redirect / to /jenkins": {
			"/",
			"/jenkins",
			`~* "^/"`,
			`
rewrite "(?i)/" /jenkins break;
proxy_pass http://upstream_balancer;`,
			false,
			"",
			false,
			true,
		},
		"redirect / to /something with sticky enabled": {
			"/",
			"/something",
			`~* "^/"`,
			`
rewrite "(?i)/" /something break;
proxy_pass http://upstream_balancer;`,
			true,
			"",
			false,
			true,
		},
		"redirect / to /something with sticky and dynamic config enabled": {
			"/",
			"/something",
			`~* "^/"`,
			`
rewrite "(?i)/" /something break;
proxy_pass http://upstream_balancer;`,
			true,
			"",
			false,
			true,
		},
		"add the X-Forwarded-Prefix header": {
			"/there",
			"/something",
			`~* "^/there"`,
			`
rewrite "(?i)/there" /something break;
proxy_set_header X-Forwarded-Prefix "/there";
proxy_pass http://upstream_balancer;`,
			true,
			"/there",
			false,
			true,
		},
		"use ~* location modifier when ingress does not use rewrite/regex target but at least one other ingress does": {
			"/something",
			"/something",
			`~* "^/something"`,
			"proxy_pass http://upstream_balancer;",
			false,
			"",
			false,
			true,
		},
	}
)

func TestBuildLuaSharedDictionaries(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := ""

	// config lua dict
	cfg := config.Configuration{
		LuaSharedDicts: map[string]int{
			"configuration_data": 10, "certificate_data": 20,
		},
	}
	actual := buildLuaSharedDictionaries(cfg, invalidType, true)

	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	servers := []*ingress.Server{
		{
			Hostname:  "foo.bar",
			Locations: []*ingress.Location{{Path: "/", LuaRestyWAF: luarestywaf.Config{}}},
		},
		{
			Hostname:  "another.host",
			Locations: []*ingress.Location{{Path: "/", LuaRestyWAF: luarestywaf.Config{}}},
		},
	}
	// returns value from config
	configuration := buildLuaSharedDictionaries(cfg, servers, false)
	if !strings.Contains(configuration, "lua_shared_dict configuration_data 10M;\n\rlua_shared_dict certificate_data 20M;") {
		t.Errorf("expected to include 'configuration_data' but got %s", configuration)
	}
	if strings.Contains(configuration, "waf_storage") {
		t.Errorf("expected to not include 'waf_storage' but got %s", configuration)
	}

	servers[1].Locations[0].LuaRestyWAF = luarestywaf.Config{Mode: "ACTIVE"}
	configuration = buildLuaSharedDictionaries(cfg, servers, false)
	if !strings.Contains(configuration, "lua_shared_dict waf_storage") {
		t.Errorf("expected to configure 'waf_storage', but got %s", configuration)
	}
	// test invalid config
	configuration = buildLuaSharedDictionaries(invalidType, servers, false)
	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v' ", expected, actual)
	}
}

func TestFormatIP(t *testing.T) {
	cases := map[string]struct {
		Input, Output string
	}{
		"ipv4-localhost": {"127.0.0.1", "127.0.0.1"},
		"ipv4-internet":  {"8.8.8.8", "8.8.8.8"},
		"ipv6-localhost": {"::1", "[::1]"},
		"ipv6-internet":  {"2001:4860:4860::8888", "[2001:4860:4860::8888]"},
		"invalid-ip":     {"nonsense", "nonsense"},
		"empty-ip":       {"", ""},
	}
	for k, tc := range cases {
		res := formatIP(tc.Input)
		if res != tc.Output {
			t.Errorf("%s: called formatIp('%s'); expected '%v' but returned '%v'", k, tc.Input, tc.Output, res)
		}
	}
}

func TestQuote(t *testing.T) {
	cases := map[interface{}]string{
		"foo":      `"foo"`,
		"\"foo\"":  `"\"foo\""`,
		"foo\nbar": `"foo\nbar"`,
		10:         `"10"`,
	}
	for input, output := range cases {
		actual := quote(input)
		if actual != output {
			t.Errorf("quote('%s'): expected '%v' but returned '%v'", input, output, actual)
		}
	}
}

func TestBuildLocation(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := "/"
	actual := buildLocation(invalidType, true)

	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	for k, tc := range tmplFuncTestcases {
		loc := &ingress.Location{
			Path:    tc.Path,
			Rewrite: rewrite.Config{Target: tc.Target},
		}

		newLoc := buildLocation(loc, tc.enforceRegex)
		if tc.Location != newLoc {
			t.Errorf("%s: expected '%v' but returned %v", k, tc.Location, newLoc)
		}
	}
}

func TestBuildProxyPass(t *testing.T) {
	defaultBackend := "upstream-name"
	defaultHost := "example.com"

	for k, tc := range tmplFuncTestcases {
		loc := &ingress.Location{
			Path:             tc.Path,
			Rewrite:          rewrite.Config{Target: tc.Target},
			Backend:          defaultBackend,
			XForwardedPrefix: tc.XForwardedPrefix,
		}

		if tc.SecureBackend {
			loc.BackendProtocol = "HTTPS"
		}

		backend := &ingress.Backend{
			Name: defaultBackend,
		}

		if tc.Sticky {
			backend.SessionAffinity = ingress.SessionAffinityConfig{
				AffinityType: "cookie",
				CookieSessionAffinity: ingress.CookieSessionAffinity{
					Locations: map[string][]string{
						defaultHost: {tc.Path},
					},
				},
			}
		}

		backends := []*ingress.Backend{backend}

		pp := buildProxyPass(defaultHost, backends, loc)
		if !strings.EqualFold(tc.ProxyPass, pp) {
			t.Errorf("%s: expected \n'%v'\nbut returned \n'%v'", k, tc.ProxyPass, pp)
		}
	}
}

func TestBuildAuthLocation(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := ""
	actual := buildAuthLocation(invalidType, "")

	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	authURL := "foo.com/auth"
	globalAuthURL := "foo.com/global-auth"

	loc := &ingress.Location{
		ExternalAuth: authreq.Config{
			URL: authURL,
		},
		Path:             "/cat",
		EnableGlobalAuth: true,
	}

	encodedAuthURL := strings.Replace(base64.URLEncoding.EncodeToString([]byte(loc.Path)), "=", "", -1)
	externalAuthPath := fmt.Sprintf("/_external-auth-%v", encodedAuthURL)

	testCases := []struct {
		title                    string
		authURL                  string
		globalAuthURL            string
		enableglobalExternalAuth bool
		expected                 string
	}{
		{"authURL, globalAuthURL and enabled", authURL, globalAuthURL, true, externalAuthPath},
		{"authURL, globalAuthURL and disabled", authURL, globalAuthURL, false, externalAuthPath},
		{"authURL, empty globalAuthURL and enabled", authURL, "", true, externalAuthPath},
		{"authURL, empty globalAuthURL and disabled", authURL, "", false, externalAuthPath},
		{"globalAuthURL and enabled", "", globalAuthURL, true, externalAuthPath},
		{"globalAuthURL and disabled", "", globalAuthURL, false, ""},
		{"all empty and enabled", "", "", true, ""},
		{"all empty and disabled", "", "", false, ""},
	}

	for _, testCase := range testCases {
		loc.ExternalAuth.URL = testCase.authURL
		loc.EnableGlobalAuth = testCase.enableglobalExternalAuth

		str := buildAuthLocation(loc, testCase.globalAuthURL)
		if str != testCase.expected {
			t.Errorf("%v: expected '%v' but returned '%v'", testCase.title, testCase.expected, str)
		}
	}
}

func TestShouldApplyGlobalAuth(t *testing.T) {

	authURL := "foo.com/auth"
	globalAuthURL := "foo.com/global-auth"

	loc := &ingress.Location{
		ExternalAuth: authreq.Config{
			URL: authURL,
		},
		Path:             "/cat",
		EnableGlobalAuth: true,
	}

	testCases := []struct {
		title                    string
		authURL                  string
		globalAuthURL            string
		enableglobalExternalAuth bool
		expected                 bool
	}{
		{"authURL, globalAuthURL and enabled", authURL, globalAuthURL, true, false},
		{"authURL, globalAuthURL and disabled", authURL, globalAuthURL, false, false},
		{"authURL, empty globalAuthURL and enabled", authURL, "", true, false},
		{"authURL, empty globalAuthURL and disabled", authURL, "", false, false},
		{"globalAuthURL and enabled", "", globalAuthURL, true, true},
		{"globalAuthURL and disabled", "", globalAuthURL, false, false},
		{"all empty and enabled", "", "", true, false},
		{"all empty and disabled", "", "", false, false},
	}

	for _, testCase := range testCases {
		loc.ExternalAuth.URL = testCase.authURL
		loc.EnableGlobalAuth = testCase.enableglobalExternalAuth

		result := shouldApplyGlobalAuth(loc, testCase.globalAuthURL)
		if result != testCase.expected {
			t.Errorf("%v: expected '%v' but returned '%v'", testCase.title, testCase.expected, result)
		}
	}
}

func TestBuildAuthResponseHeaders(t *testing.T) {
	externalAuthResponseHeaders := []string{"h1", "H-With-Caps-And-Dashes"}
	expected := []string{
		"auth_request_set $authHeader0 $upstream_http_h1;",
		"proxy_set_header 'h1' $authHeader0;",
		"auth_request_set $authHeader1 $upstream_http_h_with_caps_and_dashes;",
		"proxy_set_header 'H-With-Caps-And-Dashes' $authHeader1;",
	}

	headers := buildAuthResponseHeaders(externalAuthResponseHeaders)

	if !reflect.DeepEqual(expected, headers) {
		t.Errorf("Expected \n'%v'\nbut returned \n'%v'", expected, headers)
	}
}

func TestTemplateWithData(t *testing.T) {
	pwd, _ := os.Getwd()
	f, err := os.Open(path.Join(pwd, "../../../../test/data/config.json"))
	if err != nil {
		t.Errorf("unexpected error reading json file: %v", err)
	}
	defer f.Close()
	data, err := ioutil.ReadFile(f.Name())
	if err != nil {
		t.Error("unexpected error reading json file: ", err)
	}
	var dat config.TemplateConfig
	if err := jsoniter.ConfigCompatibleWithStandardLibrary.Unmarshal(data, &dat); err != nil {
		t.Errorf("unexpected error unmarshalling json: %v", err)
	}
	if dat.ListenPorts == nil {
		dat.ListenPorts = &config.ListenPorts{}
	}

	fs, err := file.NewFakeFS()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ngxTpl, err := NewTemplate("/etc/nginx/template/nginx.tmpl", fs)
	if err != nil {
		t.Errorf("invalid NGINX template: %v", err)
	}

	rt, err := ngxTpl.Write(dat)
	if err != nil {
		t.Errorf("invalid NGINX template: %v", err)
	}

	if !strings.Contains(string(rt), "listen [2001:db8:a0b:12f0::1]") {
		t.Errorf("invalid NGINX template, expected IPV6 listen address not present")
	}

	if !strings.Contains(string(rt), "listen [3731:54:65fe:2::a7]") {
		t.Errorf("invalid NGINX template, expected IPV6 listen address not present")
	}

	if !strings.Contains(string(rt), "listen 2.2.2.2") {
		t.Errorf("invalid NGINX template, expected IPV4 listen address not present")
	}
}

func BenchmarkTemplateWithData(b *testing.B) {
	pwd, _ := os.Getwd()
	f, err := os.Open(path.Join(pwd, "../../../../test/data/config.json"))
	if err != nil {
		b.Errorf("unexpected error reading json file: %v", err)
	}
	defer f.Close()
	data, err := ioutil.ReadFile(f.Name())
	if err != nil {
		b.Error("unexpected error reading json file: ", err)
	}
	var dat config.TemplateConfig
	if err := jsoniter.ConfigCompatibleWithStandardLibrary.Unmarshal(data, &dat); err != nil {
		b.Errorf("unexpected error unmarshalling json: %v", err)
	}

	fs, err := file.NewFakeFS()
	if err != nil {
		b.Fatalf("unexpected error: %v", err)
	}

	ngxTpl, err := NewTemplate("/etc/nginx/template/nginx.tmpl", fs)
	if err != nil {
		b.Errorf("invalid NGINX template: %v", err)
	}

	for i := 0; i < b.N; i++ {
		ngxTpl.Write(dat)
	}
}

func TestBuildDenyVariable(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := ""
	actual := buildDenyVariable(invalidType)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	a := buildDenyVariable("host1.example.com_/.well-known/acme-challenge")
	b := buildDenyVariable("host1.example.com_/.well-known/acme-challenge")
	if !reflect.DeepEqual(a, b) {
		t.Errorf("Expected '%v' but returned '%v'", a, b)
	}
}

func TestBuildByteSize(t *testing.T) {
	cases := []struct {
		value    interface{}
		isOffset bool
		expected bool
	}{
		{"1000", false, true},
		{"1000k", false, true},
		{"1m", false, true},
		{"10g", false, false},
		{" 1m ", false, true},
		{"1000kk", false, false},
		{"1000km", false, false},
		{"1mm", false, false},
		{nil, false, false},
		{"", false, false},
		{"    ", false, false},
		{"1G", true, true},
		{"1000kk", true, false},
		{"", true, false},
	}

	for _, tc := range cases {
		val := isValidByteSize(tc.value, tc.isOffset)
		if tc.expected != val {
			t.Errorf("Expected '%v' but returned '%v'", tc.expected, val)
		}
	}
}

func TestIsLocationAllowed(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := false
	actual := isLocationAllowed(invalidType)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	loc := ingress.Location{
		Denied: nil,
	}

	isAllowed := isLocationAllowed(&loc)
	if !isAllowed {
		t.Errorf("Expected '%v' but returned '%v'", true, isAllowed)
	}
}

func TestBuildForwardedFor(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := ""
	actual := buildForwardedFor(invalidType)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	inputStr := "X-Forwarded-For"
	expected = "$http_x_forwarded_for"
	actual = buildForwardedFor(inputStr)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}
}

func TestBuildResolvers(t *testing.T) {
	ipOne := net.ParseIP("192.0.0.1")
	ipTwo := net.ParseIP("2001:db8:1234:0000:0000:0000:0000:0000")
	ipList := []net.IP{ipOne, ipTwo}

	invalidType := &ingress.Ingress{}
	expected := ""
	actual := buildResolvers(invalidType, false)

	// Invalid Type for []net.IP
	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	actual = buildResolvers(ipList, invalidType)

	// Invalid Type for bool
	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	validResolver := "resolver 192.0.0.1 [2001:db8:1234::] valid=30s;"
	resolver := buildResolvers(ipList, false)

	if resolver != validResolver {
		t.Errorf("Expected '%v' but returned '%v'", validResolver, resolver)
	}

	validResolver = "resolver 192.0.0.1 valid=30s ipv6=off;"
	resolver = buildResolvers(ipList, true)

	if resolver != validResolver {
		t.Errorf("Expected '%v' but returned '%v'", validResolver, resolver)
	}
}

func TestBuildNextUpstream(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := ""
	actual := buildNextUpstream(invalidType, "")

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	cases := map[string]struct {
		NextUpstream  string
		NonIdempotent bool
		Output        string
	}{
		"default": {
			"timeout http_500 http_502",
			false,
			"timeout http_500 http_502",
		},
		"global": {
			"timeout http_500 http_502",
			true,
			"timeout http_500 http_502 non_idempotent",
		},
		"local": {
			"timeout http_500 http_502 non_idempotent",
			false,
			"timeout http_500 http_502 non_idempotent",
		},
	}

	for k, tc := range cases {
		nextUpstream := buildNextUpstream(tc.NextUpstream, tc.NonIdempotent)
		if nextUpstream != tc.Output {
			t.Errorf(
				"%s: called buildNextUpstream('%s', %v); expected '%v' but returned '%v'",
				k,
				tc.NextUpstream,
				tc.NonIdempotent,
				tc.Output,
				nextUpstream,
			)
		}
	}
}

func TestBuildRateLimit(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := []string{}
	actual := buildRateLimit(invalidType)

	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	loc := &ingress.Location{}

	loc.RateLimit.Connections.Name = "con"
	loc.RateLimit.Connections.Limit = 1

	loc.RateLimit.RPS.Name = "rps"
	loc.RateLimit.RPS.Limit = 1
	loc.RateLimit.RPS.Burst = 1

	loc.RateLimit.RPM.Name = "rpm"
	loc.RateLimit.RPM.Limit = 2
	loc.RateLimit.RPM.Burst = 2

	loc.RateLimit.LimitRateAfter = 1
	loc.RateLimit.LimitRate = 1

	validLimits := []string{
		"limit_conn con 1;",
		"limit_req zone=rps burst=1 nodelay;",
		"limit_req zone=rpm burst=2 nodelay;",
		"limit_rate_after 1k;",
		"limit_rate 1k;",
	}

	limits := buildRateLimit(loc)

	for i, limit := range limits {
		if limit != validLimits[i] {
			t.Errorf("Expected '%v' but returned '%v'", validLimits, limits)
		}
	}

	// Invalid limit
	limits = buildRateLimit(&ingress.Ingress{})
	if !reflect.DeepEqual(expected, limits) {
		t.Errorf("Expected '%v' but returned '%v'", expected, limits)
	}
}

// TODO: Needs more tests
func TestBuildRateLimitZones(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := []string{}
	actual := buildRateLimitZones(invalidType)

	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}
}

// TODO: Needs more tests
func TestFilterRateLimits(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := []ratelimit.Config{}
	actual := filterRateLimits(invalidType)

	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}
}

func TestBuildAuthSignURL(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := ""
	actual := buildAuthSignURL(invalidType)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	cases := map[string]struct {
		Input, Output string
	}{
		"default url":       {"http://google.com", "http://google.com?rd=$pass_access_scheme://$http_host$escaped_request_uri"},
		"with random field": {"http://google.com?cat=0", "http://google.com?cat=0&rd=$pass_access_scheme://$http_host$escaped_request_uri"},
		"with rd field":     {"http://google.com?cat&rd=$request", "http://google.com?cat&rd=$request"},
	}
	for k, tc := range cases {
		res := buildAuthSignURL(tc.Input)
		if res != tc.Output {
			t.Errorf("%s: called buildAuthSignURL('%s'); expected '%v' but returned '%v'", k, tc.Input, tc.Output, res)
		}
	}
}

func TestIsLocationInLocationList(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := false
	actual := isLocationInLocationList(invalidType, "")

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	testCases := []struct {
		location        *ingress.Location
		rawLocationList string
		expected        bool
	}{
		{&ingress.Location{Path: "/match"}, "/match", true},
		{&ingress.Location{Path: "/match"}, ",/match", true},
		{&ingress.Location{Path: "/match"}, "/dontmatch", false},
		{&ingress.Location{Path: "/match"}, ",/dontmatch", false},
		{&ingress.Location{Path: "/match"}, "/dontmatch,/match", true},
		{&ingress.Location{Path: "/match"}, "/dontmatch,/dontmatcheither", false},
	}

	for _, testCase := range testCases {
		result := isLocationInLocationList(testCase.location, testCase.rawLocationList)
		if result != testCase.expected {
			t.Errorf(" expected %v but return %v, path: '%s', rawLocation: '%s'", testCase.expected, result, testCase.location.Path, testCase.rawLocationList)
		}
	}
}

func TestBuildUpstreamName(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := ""
	actual := buildUpstreamName(invalidType)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	defaultBackend := "upstream-name"
	defaultHost := "example.com"

	for k, tc := range tmplFuncTestcases {
		loc := &ingress.Location{
			Path:             tc.Path,
			Rewrite:          rewrite.Config{Target: tc.Target},
			Backend:          defaultBackend,
			XForwardedPrefix: tc.XForwardedPrefix,
		}

		if tc.SecureBackend {
			loc.BackendProtocol = "HTTPS"
		}

		backend := &ingress.Backend{
			Name: defaultBackend,
		}

		expected := defaultBackend

		if tc.Sticky {
			backend.SessionAffinity = ingress.SessionAffinityConfig{
				AffinityType: "cookie",
				CookieSessionAffinity: ingress.CookieSessionAffinity{
					Locations: map[string][]string{
						defaultHost: {tc.Path},
					},
				},
			}
		}

		pp := buildUpstreamName(loc)
		if !strings.EqualFold(expected, pp) {
			t.Errorf("%s: expected \n'%v'\nbut returned \n'%v'", k, expected, pp)
		}
	}
}

func TestEscapeLiteralDollar(t *testing.T) {
	escapedPath := escapeLiteralDollar("/$")
	expected := "/${literal_dollar}"
	if escapedPath != expected {
		t.Errorf("Expected %v but returned %v", expected, escapedPath)
	}

	escapedPath = escapeLiteralDollar("/hello-$/world-$/")
	expected = "/hello-${literal_dollar}/world-${literal_dollar}/"
	if escapedPath != expected {
		t.Errorf("Expected %v but returned %v", expected, escapedPath)
	}

	leaveUnchagned := "/leave-me/unchagned"
	escapedPath = escapeLiteralDollar(leaveUnchagned)
	if escapedPath != leaveUnchagned {
		t.Errorf("Expected %v but returned %v", leaveUnchagned, escapedPath)
	}

	escapedPath = escapeLiteralDollar(false)
	expected = ""
	if escapedPath != expected {
		t.Errorf("Expected %v but returned %v", expected, escapedPath)
	}
}

func TestOpentracingPropagateContext(t *testing.T) {
	tests := map[interface{}]string{
		&ingress.Location{BackendProtocol: "HTTP"}:  "opentracing_propagate_context",
		&ingress.Location{BackendProtocol: "HTTPS"}: "opentracing_propagate_context",
		&ingress.Location{BackendProtocol: "GRPC"}:  "opentracing_grpc_propagate_context",
		&ingress.Location{BackendProtocol: "GRPCS"}: "opentracing_grpc_propagate_context",
		&ingress.Location{BackendProtocol: "AJP"}:   "opentracing_propagate_context",
		&ingress.Location{BackendProtocol: "FCGI"}:  "opentracing_propagate_context",
		"not a location": "opentracing_propagate_context",
	}

	for loc, expectedDirective := range tests {
		actualDirective := opentracingPropagateContext(loc)
		if actualDirective != expectedDirective {
			t.Errorf("Expected %v but returned %v", expectedDirective, actualDirective)
		}
	}
}

func TestGetIngressInformation(t *testing.T) {
	validIngress := &ingress.Ingress{}
	invalidIngress := "wrongtype"
	host := "host1"
	validPath := "/ok"
	invalidPath := 10

	info := getIngressInformation(invalidIngress, host, validPath)
	expected := &ingressInformation{}
	if !info.Equal(expected) {
		t.Errorf("Expected %v, but got %v", expected, info)
	}

	info = getIngressInformation(validIngress, host, invalidPath)
	if !info.Equal(expected) {
		t.Errorf("Expected %v, but got %v", expected, info)
	}

	// Setup Ingress Resource
	validIngress.Namespace = "default"
	validIngress.Name = "validIng"
	validIngress.Annotations = map[string]string{
		"ingress.annotation": "ok",
	}
	validIngress.Spec.Backend = &networking.IngressBackend{
		ServiceName: "a-svc",
	}

	info = getIngressInformation(validIngress, host, validPath)
	expected = &ingressInformation{
		Namespace: "default",
		Rule:      "validIng",
		Annotations: map[string]string{
			"ingress.annotation": "ok",
		},
		Service: "a-svc",
	}
	if !info.Equal(expected) {
		t.Errorf("Expected %v, but got %v", expected, info)
	}

	validIngress.Spec.Backend = nil
	validIngress.Spec.Rules = []networking.IngressRule{
		{
			Host: host,
			IngressRuleValue: networking.IngressRuleValue{
				HTTP: &networking.HTTPIngressRuleValue{
					Paths: []networking.HTTPIngressPath{
						{
							Path: "/ok",
							Backend: networking.IngressBackend{
								ServiceName: "b-svc",
							},
						},
					},
				},
			},
		},
		{},
	}

	info = getIngressInformation(validIngress, host, validPath)
	expected = &ingressInformation{
		Namespace: "default",
		Rule:      "validIng",
		Annotations: map[string]string{
			"ingress.annotation": "ok",
		},
		Service: "b-svc",
	}
	if !info.Equal(expected) {
		t.Errorf("Expected %v, but got %v", expected, info)
	}

	validIngress.Spec.Rules = append(validIngress.Spec.Rules, networking.IngressRule{
		Host: "host2",
		IngressRuleValue: networking.IngressRuleValue{
			HTTP: &networking.HTTPIngressRuleValue{
				Paths: []networking.HTTPIngressPath{
					{
						Path: "/ok",
						Backend: networking.IngressBackend{
							ServiceName: "c-svc",
						},
					},
				},
			},
		},
	})

	info = getIngressInformation(validIngress, host, validPath)
	if !info.Equal(expected) {
		t.Errorf("Expected %v, but got %v", expected, info)
	}

	info = getIngressInformation(validIngress, "host2", validPath)
	expected.Service = "c-svc"
	if !info.Equal(expected) {
		t.Errorf("Expected %v, but got %v", expected, info)
	}
}

func TestBuildCustomErrorLocationsPerServer(t *testing.T) {
	testCases := []struct {
		server          interface{}
		expectedResults []errorLocation
	}{
		{ // Single ingress
			&ingress.Server{Locations: []*ingress.Location{
				{
					DefaultBackendUpstreamName: "custom-default-backend-test-backend",
					CustomHTTPErrors:           []int{401, 402},
				},
			}},
			[]errorLocation{
				{
					UpstreamName: "custom-default-backend-test-backend",
					Codes:        []int{401, 402},
				},
			},
		},
		{ // Two ingresses, overlapping error codes, same backend
			&ingress.Server{Locations: []*ingress.Location{
				{
					DefaultBackendUpstreamName: "custom-default-backend-test-backend",
					CustomHTTPErrors:           []int{401, 402},
				},
				{
					DefaultBackendUpstreamName: "custom-default-backend-test-backend",
					CustomHTTPErrors:           []int{402, 403},
				},
			}},
			[]errorLocation{
				{
					UpstreamName: "custom-default-backend-test-backend",
					Codes:        []int{401, 402, 403},
				},
			},
		},
		{ // Two ingresses, overlapping error codes, different backends
			&ingress.Server{Locations: []*ingress.Location{
				{
					DefaultBackendUpstreamName: "custom-default-backend-test-one",
					CustomHTTPErrors:           []int{401, 402},
				},
				{
					DefaultBackendUpstreamName: "custom-default-backend-test-two",
					CustomHTTPErrors:           []int{402, 403},
				},
			}},
			[]errorLocation{
				{
					UpstreamName: "custom-default-backend-test-one",
					Codes:        []int{401, 402},
				},
				{
					UpstreamName: "custom-default-backend-test-two",
					Codes:        []int{402, 403},
				},
			},
		},
		{ // Many ingresses, overlapping error codes, different backends
			&ingress.Server{Locations: []*ingress.Location{
				{
					DefaultBackendUpstreamName: "custom-default-backend-test-one",
					CustomHTTPErrors:           []int{401, 402},
				},
				{
					DefaultBackendUpstreamName: "custom-default-backend-test-one",
					CustomHTTPErrors:           []int{501, 502},
				},
				{
					DefaultBackendUpstreamName: "custom-default-backend-test-two",
					CustomHTTPErrors:           []int{409, 410},
				},
				{
					DefaultBackendUpstreamName: "custom-default-backend-test-two",
					CustomHTTPErrors:           []int{504, 505},
				},
			}},
			[]errorLocation{
				{
					UpstreamName: "custom-default-backend-test-one",
					Codes:        []int{401, 402, 501, 502},
				},
				{
					UpstreamName: "custom-default-backend-test-two",
					Codes:        []int{409, 410, 504, 505},
				},
			},
		},
	}

	for _, c := range testCases {
		response := buildCustomErrorLocationsPerServer(c.server)
		if results, ok := response.([]errorLocation); ok {
			if !reflect.DeepEqual(c.expectedResults, results) {
				t.Errorf("Expected %+v but got %+v", c.expectedResults, results)
			}
		} else {
			t.Error("Unable to convert to []errorLocation")
		}
	}
}

func TestProxySetHeader(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := "proxy_set_header"
	actual := proxySetHeader(invalidType)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	grpcBackend := &ingress.Location{
		BackendProtocol: "GRPC",
	}

	expected = "grpc_set_header"
	actual = proxySetHeader(grpcBackend)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}
}

func TestBuildInfluxDB(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := ""
	actual := buildInfluxDB(invalidType)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	cfg := influxdb.Config{
		InfluxDBEnabled:     true,
		InfluxDBServerName:  "ok.com",
		InfluxDBHost:        "host.com",
		InfluxDBPort:        "5252",
		InfluxDBMeasurement: "ok",
	}
	expected = "influxdb server_name=ok.com host=host.com port=5252 measurement=ok enabled=true;"
	actual = buildInfluxDB(cfg)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}
}

func TestBuildOpenTracing(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := ""
	actual := buildOpentracing(invalidType)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	cfgJaeger := config.Configuration{
		EnableOpentracing:   true,
		JaegerCollectorHost: "jaeger-host.com",
	}
	expected = "opentracing_load_tracer /usr/local/lib/libjaegertracing_plugin.so /etc/nginx/opentracing.json;\r\n"
	actual = buildOpentracing(cfgJaeger)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	cfgZipkin := config.Configuration{
		EnableOpentracing:   true,
		ZipkinCollectorHost: "zipkin-host.com",
	}
	expected = "opentracing_load_tracer /usr/local/lib/libzipkin_opentracing.so /etc/nginx/opentracing.json;\r\n"
	actual = buildOpentracing(cfgZipkin)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	cfgDatadog := config.Configuration{
		EnableOpentracing:    true,
		DatadogCollectorHost: "datadog-host.com",
	}
	expected = "opentracing_load_tracer /usr/local/lib/libdd_opentracing.so /etc/nginx/opentracing.json;\r\n"
	actual = buildOpentracing(cfgDatadog)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

}

func TestEnforceRegexModifier(t *testing.T) {
	invalidType := &ingress.Ingress{}
	expected := false
	actual := enforceRegexModifier(invalidType)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	locs := []*ingress.Location{
		{
			Rewrite: rewrite.Config{
				Target:   "/alright",
				UseRegex: true,
			},
			Path: "/ok",
		},
	}
	expected = true
	actual = enforceRegexModifier(locs)

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}
}

func TestStripLocationModifer(t *testing.T) {
	expected := "ok.com"
	actual := stripLocationModifer("~*ok.com")

	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}
}

func TestShouldLoadModSecurityModule(t *testing.T) {
	// ### Invalid argument type tests ###
	// The first tests should return false.
	expected := false

	invalidType := &ingress.Ingress{}
	actual := shouldLoadModSecurityModule(config.Configuration{}, invalidType)
	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	actual = shouldLoadModSecurityModule(invalidType, []*ingress.Server{})
	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	// ### Functional tests ###
	actual = shouldLoadModSecurityModule(config.Configuration{}, []*ingress.Server{})
	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	// All further tests should return true.
	expected = true

	configuration := config.Configuration{EnableModsecurity: true}
	actual = shouldLoadModSecurityModule(configuration, []*ingress.Server{})
	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	servers := []*ingress.Server{
		{
			Locations: []*ingress.Location{
				{
					ModSecurity: modsecurity.Config{
						Enable: true,
					},
				},
			},
		},
	}
	actual = shouldLoadModSecurityModule(config.Configuration{}, servers)
	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}

	actual = shouldLoadModSecurityModule(configuration, servers)
	if expected != actual {
		t.Errorf("Expected '%v' but returned '%v'", expected, actual)
	}
}
