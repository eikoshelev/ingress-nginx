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
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	text_template "text/template"
	"time"

	"github.com/pkg/errors"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"

	"k8s.io/ingress-nginx/internal/file"
	"k8s.io/ingress-nginx/internal/ingress"
	"k8s.io/ingress-nginx/internal/ingress/annotations/influxdb"
	"k8s.io/ingress-nginx/internal/ingress/annotations/ratelimit"
	"k8s.io/ingress-nginx/internal/ingress/controller/config"
	ing_net "k8s.io/ingress-nginx/internal/net"
)

const (
	slash         = "/"
	nonIdempotent = "non_idempotent"
	defBufferSize = 65535
)

// TemplateWriter is the interface to render a template
type TemplateWriter interface {
	Write(conf config.TemplateConfig) ([]byte, error)
}

// Template ...
type Template struct {
	tmpl *text_template.Template
	//fw   watch.FileWatcher
	bp *BufferPool
}

//NewTemplate returns a new Template instance or an
//error if the specified template file contains errors
func NewTemplate(file string, fs file.Filesystem) (*Template, error) {
	data, err := fs.ReadFile(file)
	if err != nil {
		return nil, errors.Wrapf(err, "unexpected error reading template %v", file)
	}

	tmpl, err := text_template.New("nginx.tmpl").Funcs(funcMap).Parse(string(data))
	if err != nil {
		return nil, err
	}

	return &Template{
		tmpl: tmpl,
		bp:   NewBufferPool(defBufferSize),
	}, nil
}

// Write populates a buffer using a template with NGINX configuration
// and the servers and upstreams created by Ingress rules
func (t *Template) Write(conf config.TemplateConfig) ([]byte, error) {
	tmplBuf := t.bp.Get()
	defer t.bp.Put(tmplBuf)

	outCmdBuf := t.bp.Get()
	defer t.bp.Put(outCmdBuf)

	// TODO: remove once we found a fix for coredump running luarocks install lrexlib
	if runtime.GOARCH == "arm" {
		conf.Cfg.DisableLuaRestyWAF = true
	}

	if klog.V(3) {
		b, err := json.Marshal(conf)
		if err != nil {
			klog.Errorf("unexpected error: %v", err)
		}
		klog.Infof("NGINX configuration: %v", string(b))
	}

	err := t.tmpl.Execute(tmplBuf, conf)
	if err != nil {
		return nil, err
	}

	// squeezes multiple adjacent empty lines to be single
	// spaced this is to avoid the use of regular expressions
	cmd := exec.Command("/ingress-controller/clean-nginx-conf.sh")
	cmd.Stdin = tmplBuf
	cmd.Stdout = outCmdBuf
	if err := cmd.Run(); err != nil {
		klog.Warningf("unexpected error cleaning template: %v", err)
		return tmplBuf.Bytes(), nil
	}

	return outCmdBuf.Bytes(), nil
}

var (
	funcMap = text_template.FuncMap{
		"empty": func(input interface{}) bool {
			check, ok := input.(string)
			if ok {
				return len(check) == 0
			}
			return true
		},
		"escapeLiteralDollar":        escapeLiteralDollar,
		"shouldConfigureLuaRestyWAF": shouldConfigureLuaRestyWAF,
		"buildLuaSharedDictionaries": buildLuaSharedDictionaries,
		"buildLocation":              buildLocation,
		"buildAuthLocation":          buildAuthLocation,
		"shouldApplyGlobalAuth":      shouldApplyGlobalAuth,
		"buildAuthResponseHeaders":   buildAuthResponseHeaders,
		"buildProxyPass":             buildProxyPass,
		"filterRateLimits":           filterRateLimits,
		"buildRateLimitZones":        buildRateLimitZones,
		"buildRateLimit":             buildRateLimit,
		"configForLua":               configForLua,
		"locationConfigForLua":       locationConfigForLua,
		"buildResolvers":             buildResolvers,
		"buildUpstreamName":          buildUpstreamName,
		"isLocationInLocationList":   isLocationInLocationList,
		"isLocationAllowed":          isLocationAllowed,
		"buildLogFormatUpstream":     buildLogFormatUpstream,
		"buildDenyVariable":          buildDenyVariable,
		"getenv":                     os.Getenv,
		"contains":                   strings.Contains,
		"hasPrefix":                  strings.HasPrefix,
		"hasSuffix":                  strings.HasSuffix,
		"trimSpace":                  strings.TrimSpace,
		"toUpper":                    strings.ToUpper,
		"toLower":                    strings.ToLower,
		"formatIP":                   formatIP,
		"quote":                      quote,
		"buildNextUpstream":          buildNextUpstream,
		"getIngressInformation":      getIngressInformation,
		"serverConfig": func(all config.TemplateConfig, server *ingress.Server) interface{} {
			return struct{ First, Second interface{} }{all, server}
		},
		"isValidByteSize":                    isValidByteSize,
		"buildForwardedFor":                  buildForwardedFor,
		"buildAuthSignURL":                   buildAuthSignURL,
		"buildOpentracing":                   buildOpentracing,
		"proxySetHeader":                     proxySetHeader,
		"buildInfluxDB":                      buildInfluxDB,
		"enforceRegexModifier":               enforceRegexModifier,
		"stripLocationModifer":               stripLocationModifer,
		"buildCustomErrorDeps":               buildCustomErrorDeps,
		"opentracingPropagateContext":        opentracingPropagateContext,
		"buildCustomErrorLocationsPerServer": buildCustomErrorLocationsPerServer,
		"shouldLoadModSecurityModule":        shouldLoadModSecurityModule,
		"buildHTTPListener":                  buildHTTPListener,
		"buildHTTPSListener":                 buildHTTPSListener,
	}
)

// escapeLiteralDollar will replace the $ character with ${literal_dollar}
// which is made to work via the following configuration in the http section of
// the template:
// geo $literal_dollar {
//     default "$";
// }
func escapeLiteralDollar(input interface{}) string {
	inputStr, ok := input.(string)
	if !ok {
		return ""
	}
	return strings.Replace(inputStr, `$`, `${literal_dollar}`, -1)
}

// formatIP will wrap IPv6 addresses in [] and return IPv4 addresses
// without modification. If the input cannot be parsed as an IP address
// it is returned without modification.
func formatIP(input string) string {
	ip := net.ParseIP(input)
	if ip == nil {
		return input
	}
	if v4 := ip.To4(); v4 != nil {
		return input
	}
	return fmt.Sprintf("[%s]", input)
}

func quote(input interface{}) string {
	var inputStr string
	switch input := input.(type) {
	case string:
		inputStr = input
		break
	case fmt.Stringer:
		inputStr = input.String()
		break
	default:
		inputStr = fmt.Sprintf("%v", input)
	}
	return fmt.Sprintf("%q", inputStr)
}

func shouldConfigureLuaRestyWAF(disableLuaRestyWAF bool, mode string) bool {
	if !disableLuaRestyWAF && len(mode) > 0 {
		return true
	}

	return false
}

func buildLuaSharedDictionaries(c interface{}, s interface{}, disableLuaRestyWAF bool) string {
	var out []string
	// Load config
	cfg, ok := c.(config.Configuration)
	if !ok {
		klog.Errorf("expected a 'config.Configuration' type but %T was returned", c)
		return ""
	}
	servers, ok := s.([]*ingress.Server)
	if !ok {
		klog.Errorf("expected an '[]*ingress.Server' type but %T was returned", s)
		return ""
	}
	// check if config contains lua "lua_configuration_data" value otherwise, use default
	cfgData, ok := cfg.LuaSharedDicts["configuration_data"]
	if !ok {
		cfgData = 15
	}
	out = append(out, fmt.Sprintf("lua_shared_dict configuration_data %dM", cfgData))

	// check if config contains "lua_certificate_data" value otherwise, use default
	certData, ok := cfg.LuaSharedDicts["certificate_data"]
	if !ok {
		certData = 16
	}
	out = append(out, fmt.Sprintf("lua_shared_dict certificate_data %dM", certData))
	if !disableLuaRestyWAF {
		luaRestyWAFEnabled := func() bool {
			for _, server := range servers {
				for _, location := range server.Locations {
					if len(location.LuaRestyWAF.Mode) > 0 {
						return true
					}
				}
			}
			return false
		}()
		if luaRestyWAFEnabled {
			out = append(out, "lua_shared_dict waf_storage 64M")
		}
	}
	return strings.Join(out, ";\n\r") + ";"
}

// configForLua returns some general configuration as Lua table represented as string
func configForLua(input interface{}) string {
	all, ok := input.(config.TemplateConfig)
	if !ok {
		klog.Errorf("expected a 'config.TemplateConfig' type but %T was given", input)
		return "{}"
	}

	return fmt.Sprintf(`{
		use_forwarded_headers = %t,
		is_ssl_passthrough_enabled = %t,
		http_redirect_code = %v,
		listen_ports = { ssl_proxy = "%v", https = "%v" },
	}`, all.Cfg.UseForwardedHeaders, all.IsSSLPassthroughEnabled, all.Cfg.HTTPRedirectCode, all.ListenPorts.SSLProxy, all.ListenPorts.HTTPS)
}

// locationConfigForLua formats some location specific configuration into Lua table represented as string
func locationConfigForLua(l interface{}, s interface{}, a interface{}) string {
	location, ok := l.(*ingress.Location)
	if !ok {
		klog.Errorf("expected an '*ingress.Location' type but %T was given", l)
		return "{}"
	}

	server, ok := s.(*ingress.Server)
	if !ok {
		klog.Errorf("expected an '*ingress.Server' type but %T was given", s)
		return "{}"
	}

	all, ok := a.(config.TemplateConfig)
	if !ok {
		klog.Errorf("expected a 'config.TemplateConfig' type but %T was given", a)
		return "{}"
	}

	forceSSLRedirect := location.Rewrite.ForceSSLRedirect || len(server.SSLCert.PemFileName) > 0 && location.Rewrite.SSLRedirect
	forceSSLRedirect = forceSSLRedirect && !isLocationInLocationList(l, all.Cfg.NoTLSRedirectLocations)

	return fmt.Sprintf(`{
		force_ssl_redirect = %t,
		use_port_in_redirects = %t,
	}`, forceSSLRedirect, location.UsePortInRedirects)
}

// buildResolvers returns the resolvers reading the /etc/resolv.conf file
func buildResolvers(res interface{}, disableIpv6 interface{}) string {
	// NGINX need IPV6 addresses to be surrounded by brackets
	nss, ok := res.([]net.IP)
	if !ok {
		klog.Errorf("expected a '[]net.IP' type but %T was returned", res)
		return ""
	}
	no6, ok := disableIpv6.(bool)
	if !ok {
		klog.Errorf("expected a 'bool' type but %T was returned", disableIpv6)
		return ""
	}

	if len(nss) == 0 {
		return ""
	}

	r := []string{"resolver"}
	for _, ns := range nss {
		if ing_net.IsIPV6(ns) {
			if no6 {
				continue
			}
			r = append(r, fmt.Sprintf("[%v]", ns))
		} else {
			r = append(r, fmt.Sprintf("%v", ns))
		}
	}
	r = append(r, "valid=30s")

	if no6 {
		r = append(r, "ipv6=off")
	}

	return strings.Join(r, " ") + ";"
}

func needsRewrite(location *ingress.Location) bool {
	if len(location.Rewrite.Target) > 0 && location.Rewrite.Target != location.Path {
		return true
	}
	return false
}

func stripLocationModifer(path string) string {
	return strings.TrimLeft(path, "~* ")
}

// enforceRegexModifier checks if the "rewrite-target" or "use-regex" annotation
// is used on any location path within a server
func enforceRegexModifier(input interface{}) bool {
	locations, ok := input.([]*ingress.Location)
	if !ok {
		klog.Errorf("expected an '[]*ingress.Location' type but %T was returned", input)
		return false
	}

	for _, location := range locations {
		if needsRewrite(location) || location.Rewrite.UseRegex {
			return true
		}
	}
	return false
}

// buildLocation produces the location string, if the ingress has redirects
// (specified through the nginx.ingress.kubernetes.io/rewrite-target annotation)
func buildLocation(input interface{}, enforceRegex bool) string {
	location, ok := input.(*ingress.Location)
	if !ok {
		klog.Errorf("expected an '*ingress.Location' type but %T was returned", input)
		return slash
	}

	path := location.Path
	if enforceRegex {
		return fmt.Sprintf(`~* "^%s"`, path)
	}
	return path
}

func buildAuthLocation(input interface{}, globalExternalAuthURL string) string {
	location, ok := input.(*ingress.Location)
	if !ok {
		klog.Errorf("expected an '*ingress.Location' type but %T was returned", input)
		return ""
	}

	if (location.ExternalAuth.URL == "") && (!shouldApplyGlobalAuth(input, globalExternalAuthURL)) {
		return ""
	}

	str := base64.URLEncoding.EncodeToString([]byte(location.Path))
	// removes "=" after encoding
	str = strings.Replace(str, "=", "", -1)
	return fmt.Sprintf("/_external-auth-%v", str)
}

// shouldApplyGlobalAuth returns true only in case when ExternalAuth.URL is not set and
// GlobalExternalAuth is set and enabled
func shouldApplyGlobalAuth(input interface{}, globalExternalAuthURL string) bool {
	location, ok := input.(*ingress.Location)
	if !ok {
		klog.Errorf("expected an '*ingress.Location' type but %T was returned", input)
	}

	if (location.ExternalAuth.URL == "") && (globalExternalAuthURL != "") && (location.EnableGlobalAuth) {
		return true
	}

	return false
}

func buildAuthResponseHeaders(headers []string) []string {
	res := []string{}

	if len(headers) == 0 {
		return res
	}

	for i, h := range headers {
		hvar := strings.ToLower(h)
		hvar = strings.NewReplacer("-", "_").Replace(hvar)
		res = append(res, fmt.Sprintf("auth_request_set $authHeader%v $upstream_http_%v;", i, hvar))
		res = append(res, fmt.Sprintf("proxy_set_header '%v' $authHeader%v;", h, i))
	}
	return res
}

func buildLogFormatUpstream(input interface{}) string {
	cfg, ok := input.(config.Configuration)
	if !ok {
		klog.Errorf("expected a 'config.Configuration' type but %T was returned", input)
		return ""
	}

	return cfg.BuildLogFormatUpstream()
}

// buildProxyPass produces the proxy pass string, if the ingress has redirects
// (specified through the nginx.ingress.kubernetes.io/rewrite-target annotation)
// If the annotation nginx.ingress.kubernetes.io/add-base-url:"true" is specified it will
// add a base tag in the head of the response from the service
func buildProxyPass(host string, b interface{}, loc interface{}) string {
	backends, ok := b.([]*ingress.Backend)
	if !ok {
		klog.Errorf("expected an '[]*ingress.Backend' type but %T was returned", b)
		return ""
	}

	location, ok := loc.(*ingress.Location)
	if !ok {
		klog.Errorf("expected a '*ingress.Location' type but %T was returned", loc)
		return ""
	}

	path := location.Path
	proto := "http://"

	proxyPass := "proxy_pass"

	switch location.BackendProtocol {
	case "HTTPS":
		proto = "https://"
	case "GRPC":
		proto = "grpc://"
		proxyPass = "grpc_pass"
	case "GRPCS":
		proto = "grpcs://"
		proxyPass = "grpc_pass"
	case "AJP":
		proto = ""
		proxyPass = "ajp_pass"
	case "FCGI":
		proto = ""
		proxyPass = "fastcgi_pass"
	}

	upstreamName := "upstream_balancer"

	for _, backend := range backends {
		if backend.Name == location.Backend {
			if backend.SSLPassthrough {
				proto = "https://"

				if location.BackendProtocol == "GRPCS" {
					proto = "grpcs://"
				}
			}

			break
		}
	}

	// defProxyPass returns the default proxy_pass, just the name of the upstream
	defProxyPass := fmt.Sprintf("%v %s%s;", proxyPass, proto, upstreamName)

	// if the path in the ingress rule is equals to the target: no special rewrite
	if path == location.Rewrite.Target {
		return defProxyPass
	}

	if len(location.Rewrite.Target) > 0 {
		var xForwardedPrefix string

		if len(location.XForwardedPrefix) > 0 {
			xForwardedPrefix = fmt.Sprintf("proxy_set_header X-Forwarded-Prefix \"%s\";\n", location.XForwardedPrefix)
		}

		return fmt.Sprintf(`
rewrite "(?i)%s" %s break;
%v%v %s%s;`, path, location.Rewrite.Target, xForwardedPrefix, proxyPass, proto, upstreamName)
	}

	// default proxy_pass
	return defProxyPass
}

// TODO: Needs Unit Tests
func filterRateLimits(input interface{}) []ratelimit.Config {
	ratelimits := []ratelimit.Config{}
	found := sets.String{}

	servers, ok := input.([]*ingress.Server)
	if !ok {
		klog.Errorf("expected a '[]ratelimit.RateLimit' type but %T was returned", input)
		return ratelimits
	}
	for _, server := range servers {
		for _, loc := range server.Locations {
			if loc.RateLimit.ID != "" && !found.Has(loc.RateLimit.ID) {
				found.Insert(loc.RateLimit.ID)
				ratelimits = append(ratelimits, loc.RateLimit)
			}
		}
	}
	return ratelimits
}

// TODO: Needs Unit Tests
// buildRateLimitZones produces an array of limit_conn_zone in order to allow
// rate limiting of request. Each Ingress rule could have up to three zones, one
// for connection limit by IP address, one for limiting requests per minute, and
// one for limiting requests per second.
func buildRateLimitZones(input interface{}) []string {
	zones := sets.String{}

	servers, ok := input.([]*ingress.Server)
	if !ok {
		klog.Errorf("expected a '[]*ingress.Server' type but %T was returned", input)
		return zones.List()
	}

	for _, server := range servers {
		for _, loc := range server.Locations {
			if loc.RateLimit.Connections.Limit > 0 {
				zone := fmt.Sprintf("limit_conn_zone $limit_%s zone=%v:%vm;",
					loc.RateLimit.ID,
					loc.RateLimit.Connections.Name,
					loc.RateLimit.Connections.SharedSize)
				if !zones.Has(zone) {
					zones.Insert(zone)
				}
			}

			if loc.RateLimit.RPM.Limit > 0 {
				zone := fmt.Sprintf("limit_req_zone $limit_%s zone=%v:%vm rate=%vr/m;",
					loc.RateLimit.ID,
					loc.RateLimit.RPM.Name,
					loc.RateLimit.RPM.SharedSize,
					loc.RateLimit.RPM.Limit)
				if !zones.Has(zone) {
					zones.Insert(zone)
				}
			}

			if loc.RateLimit.RPS.Limit > 0 {
				zone := fmt.Sprintf("limit_req_zone $limit_%s zone=%v:%vm rate=%vr/s;",
					loc.RateLimit.ID,
					loc.RateLimit.RPS.Name,
					loc.RateLimit.RPS.SharedSize,
					loc.RateLimit.RPS.Limit)
				if !zones.Has(zone) {
					zones.Insert(zone)
				}
			}
		}
	}

	return zones.List()
}

// buildRateLimit produces an array of limit_req to be used inside the Path of
// Ingress rules. The order: connections by IP first, then RPS, and RPM last.
func buildRateLimit(input interface{}) []string {
	limits := []string{}

	loc, ok := input.(*ingress.Location)
	if !ok {
		klog.Errorf("expected an '*ingress.Location' type but %T was returned", input)
		return limits
	}

	if loc.RateLimit.Connections.Limit > 0 {
		limit := fmt.Sprintf("limit_conn %v %v;",
			loc.RateLimit.Connections.Name, loc.RateLimit.Connections.Limit)
		limits = append(limits, limit)
	}

	if loc.RateLimit.RPS.Limit > 0 {
		limit := fmt.Sprintf("limit_req zone=%v burst=%v nodelay;",
			loc.RateLimit.RPS.Name, loc.RateLimit.RPS.Burst)
		limits = append(limits, limit)
	}

	if loc.RateLimit.RPM.Limit > 0 {
		limit := fmt.Sprintf("limit_req zone=%v burst=%v nodelay;",
			loc.RateLimit.RPM.Name, loc.RateLimit.RPM.Burst)
		limits = append(limits, limit)
	}

	if loc.RateLimit.LimitRateAfter > 0 {
		limit := fmt.Sprintf("limit_rate_after %vk;",
			loc.RateLimit.LimitRateAfter)
		limits = append(limits, limit)
	}

	if loc.RateLimit.LimitRate > 0 {
		limit := fmt.Sprintf("limit_rate %vk;",
			loc.RateLimit.LimitRate)
		limits = append(limits, limit)
	}

	return limits
}

func isLocationInLocationList(location interface{}, rawLocationList string) bool {
	loc, ok := location.(*ingress.Location)
	if !ok {
		klog.Errorf("expected an '*ingress.Location' type but %T was returned", location)
		return false
	}

	locationList := strings.Split(rawLocationList, ",")

	for _, locationListItem := range locationList {
		locationListItem = strings.Trim(locationListItem, " ")
		if locationListItem == "" {
			continue
		}
		if strings.HasPrefix(loc.Path, locationListItem) {
			return true
		}
	}

	return false
}

func isLocationAllowed(input interface{}) bool {
	loc, ok := input.(*ingress.Location)
	if !ok {
		klog.Errorf("expected an '*ingress.Location' type but %T was returned", input)
		return false
	}

	return loc.Denied == nil
}

var (
	denyPathSlugMap = map[string]string{}
)

// buildDenyVariable returns a nginx variable for a location in a
// server to be used in the whitelist check
// This method uses a unique id generator library to reduce the
// size of the string to be used as a variable in nginx to avoid
// issue with the size of the variable bucket size directive
func buildDenyVariable(a interface{}) string {
	l, ok := a.(string)
	if !ok {
		klog.Errorf("expected a 'string' type but %T was returned", a)
		return ""
	}

	if _, ok := denyPathSlugMap[l]; !ok {
		denyPathSlugMap[l] = randomString()
	}

	return fmt.Sprintf("$deny_%v", denyPathSlugMap[l])
}

func buildUpstreamName(loc interface{}) string {
	location, ok := loc.(*ingress.Location)
	if !ok {
		klog.Errorf("expected a '*ingress.Location' type but %T was returned", loc)
		return ""
	}

	upstreamName := location.Backend

	return upstreamName
}

func buildNextUpstream(i, r interface{}) string {
	nextUpstream, ok := i.(string)
	if !ok {
		klog.Errorf("expected a 'string' type but %T was returned", i)
		return ""
	}

	retryNonIdempotent := r.(bool)

	parts := strings.Split(nextUpstream, " ")

	nextUpstreamCodes := make([]string, 0, len(parts))
	for _, v := range parts {
		if v != "" && v != nonIdempotent {
			nextUpstreamCodes = append(nextUpstreamCodes, v)
		}

		if v == nonIdempotent {
			retryNonIdempotent = true
		}
	}

	if retryNonIdempotent {
		nextUpstreamCodes = append(nextUpstreamCodes, nonIdempotent)
	}

	return strings.Join(nextUpstreamCodes, " ")
}

// refer to http://nginx.org/en/docs/syntax.html
// Nginx differentiates between size and offset
// offset directives support gigabytes in addition
var nginxSizeRegex = regexp.MustCompile("^[0-9]+[kKmM]{0,1}$")
var nginxOffsetRegex = regexp.MustCompile("^[0-9]+[kKmMgG]{0,1}$")

// isValidByteSize validates size units valid in nginx
// http://nginx.org/en/docs/syntax.html
func isValidByteSize(input interface{}, isOffset bool) bool {
	s, ok := input.(string)
	if !ok {
		klog.Errorf("expected an 'string' type but %T was returned", input)
		return false
	}

	s = strings.TrimSpace(s)
	if s == "" {
		klog.V(2).Info("empty byte size, hence it will not be set")
		return false
	}

	if isOffset {
		return nginxOffsetRegex.MatchString(s)
	}

	return nginxSizeRegex.MatchString(s)
}

type ingressInformation struct {
	Namespace   string
	Rule        string
	Service     string
	Annotations map[string]string
}

func (info *ingressInformation) Equal(other *ingressInformation) bool {
	if info.Namespace != other.Namespace {
		return false
	}
	if info.Rule != other.Rule {
		return false
	}
	if info.Service != other.Service {
		return false
	}
	if !reflect.DeepEqual(info.Annotations, other.Annotations) {
		return false
	}

	return true
}

func getIngressInformation(i, h, p interface{}) *ingressInformation {
	ing, ok := i.(*ingress.Ingress)
	if !ok {
		klog.Errorf("expected an '*ingress.Ingress' type but %T was returned", i)
		return &ingressInformation{}
	}

	hostname, ok := h.(string)
	if !ok {
		klog.Errorf("expected a 'string' type but %T was returned", h)
		return &ingressInformation{}
	}

	path, ok := p.(string)
	if !ok {
		klog.Errorf("expected a 'string' type but %T was returned", p)
		return &ingressInformation{}
	}

	if ing == nil {
		return &ingressInformation{}
	}

	info := &ingressInformation{
		Namespace:   ing.GetNamespace(),
		Rule:        ing.GetName(),
		Annotations: ing.Annotations,
	}

	if ing.Spec.Backend != nil {
		info.Service = ing.Spec.Backend.ServiceName
	}

	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}

		if hostname != "" && hostname != rule.Host {
			continue
		}

		for _, rPath := range rule.HTTP.Paths {
			if path == rPath.Path {
				info.Service = rPath.Backend.ServiceName
				return info
			}
		}
	}

	return info
}

func buildForwardedFor(input interface{}) string {
	s, ok := input.(string)
	if !ok {
		klog.Errorf("expected a 'string' type but %T was returned", input)
		return ""
	}

	ffh := strings.Replace(s, "-", "_", -1)
	ffh = strings.ToLower(ffh)
	return fmt.Sprintf("$http_%v", ffh)
}

func buildAuthSignURL(input interface{}) string {
	s, ok := input.(string)
	if !ok {
		klog.Errorf("expected an 'string' type but %T was returned", input)
		return ""
	}

	u, _ := url.Parse(s)
	q := u.Query()
	if len(q) == 0 {
		return fmt.Sprintf("%v?rd=$pass_access_scheme://$http_host$escaped_request_uri", s)
	}

	if q.Get("rd") != "" {
		return s
	}

	return fmt.Sprintf("%v&rd=$pass_access_scheme://$http_host$escaped_request_uri", s)
}

var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func init() {
	rand.Seed(time.Now().UnixNano())
}

func randomString() string {
	b := make([]rune, 32)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}

	return string(b)
}

func buildOpentracing(input interface{}) string {
	cfg, ok := input.(config.Configuration)
	if !ok {
		klog.Errorf("expected a 'config.Configuration' type but %T was returned", input)
		return ""
	}

	if !cfg.EnableOpentracing {
		return ""
	}

	buf := bytes.NewBufferString("")
	if cfg.ZipkinCollectorHost != "" {
		buf.WriteString("opentracing_load_tracer /usr/local/lib/libzipkin_opentracing.so /etc/nginx/opentracing.json;")
	} else if cfg.JaegerCollectorHost != "" {
		if runtime.GOARCH == "arm" {
			buf.WriteString("# Jaeger tracer is not available for ARM https://github.com/jaegertracing/jaeger-client-cpp/issues/151")
		} else {
			buf.WriteString("opentracing_load_tracer /usr/local/lib/libjaegertracing_plugin.so /etc/nginx/opentracing.json;")
		}
	} else if cfg.DatadogCollectorHost != "" {
		buf.WriteString("opentracing_load_tracer /usr/local/lib/libdd_opentracing.so /etc/nginx/opentracing.json;")
	}

	buf.WriteString("\r\n")

	return buf.String()
}

// buildInfluxDB produces the single line configuration
// needed by the InfluxDB module to send request's metrics
// for the current resource
func buildInfluxDB(input interface{}) string {
	cfg, ok := input.(influxdb.Config)
	if !ok {
		klog.Errorf("expected an 'influxdb.Config' type but %T was returned", input)
		return ""
	}

	if !cfg.InfluxDBEnabled {
		return ""
	}

	return fmt.Sprintf(
		"influxdb server_name=%s host=%s port=%s measurement=%s enabled=true;",
		cfg.InfluxDBServerName,
		cfg.InfluxDBHost,
		cfg.InfluxDBPort,
		cfg.InfluxDBMeasurement,
	)
}

func proxySetHeader(loc interface{}) string {
	location, ok := loc.(*ingress.Location)
	if !ok {
		klog.Errorf("expected a '*ingress.Location' type but %T was returned", loc)
		return "proxy_set_header"
	}

	if location.BackendProtocol == "GRPC" || location.BackendProtocol == "GRPCS" {
		return "grpc_set_header"
	}

	return "proxy_set_header"
}

// buildCustomErrorDeps is a utility function returning a struct wrapper with
// the data required to build the 'CUSTOM_ERRORS' template
func buildCustomErrorDeps(upstreamName string, errorCodes []int, enableMetrics bool) interface{} {
	return struct {
		UpstreamName  string
		ErrorCodes    []int
		EnableMetrics bool
	}{
		UpstreamName:  upstreamName,
		ErrorCodes:    errorCodes,
		EnableMetrics: enableMetrics,
	}
}

type errorLocation struct {
	UpstreamName string
	Codes        []int
}

// buildCustomErrorLocationsPerServer is a utility function which will collect all
// custom error codes for all locations of a server block, deduplicates them,
// and returns a set which is unique by default-upstream and error code. It returns an array
// of errorLocations, each of which contain the upstream name and a list of
// error codes for that given upstream, so that sufficiently unique
// @custom error location blocks can be created in the template
func buildCustomErrorLocationsPerServer(input interface{}) interface{} {
	server, ok := input.(*ingress.Server)
	if !ok {
		klog.Errorf("expected a '*ingress.Server' type but %T was returned", input)
		return nil
	}

	codesMap := make(map[string]map[int]bool)
	for _, loc := range server.Locations {
		backendUpstream := loc.DefaultBackendUpstreamName

		var dedupedCodes map[int]bool
		if existingMap, ok := codesMap[backendUpstream]; ok {
			dedupedCodes = existingMap
		} else {
			dedupedCodes = make(map[int]bool)
		}

		for _, code := range loc.CustomHTTPErrors {
			dedupedCodes[code] = true
		}
		codesMap[backendUpstream] = dedupedCodes
	}

	errorLocations := []errorLocation{}

	for upstream, dedupedCodes := range codesMap {
		codesForUpstream := []int{}
		for code := range dedupedCodes {
			codesForUpstream = append(codesForUpstream, code)
		}
		sort.Ints(codesForUpstream)
		errorLocations = append(errorLocations, errorLocation{
			UpstreamName: upstream,
			Codes:        codesForUpstream,
		})
	}

	sort.Slice(errorLocations, func(i, j int) bool {
		return errorLocations[i].UpstreamName < errorLocations[j].UpstreamName
	})

	return errorLocations
}

func opentracingPropagateContext(loc interface{}) string {
	location, ok := loc.(*ingress.Location)
	if !ok {
		klog.Errorf("expected a '*ingress.Location' type but %T was returned", loc)
		return "opentracing_propagate_context"
	}

	if location.BackendProtocol == "GRPC" || location.BackendProtocol == "GRPCS" {
		return "opentracing_grpc_propagate_context"
	}

	return "opentracing_propagate_context"
}

// shouldLoadModSecurityModule determines whether or not the ModSecurity module needs to be loaded.
// First, it checks if `enable-modsecurity` is set in the ConfigMap. If it is not, it iterates over all locations to
// check if ModSecurity is enabled by the annotation `nginx.ingress.kubernetes.io/enable-modsecurity`.
func shouldLoadModSecurityModule(c interface{}, s interface{}) bool {
	cfg, ok := c.(config.Configuration)
	if !ok {
		klog.Errorf("expected a 'config.Configuration' type but %T was returned", c)
		return false
	}

	servers, ok := s.([]*ingress.Server)
	if !ok {
		klog.Errorf("expected an '[]*ingress.Server' type but %T was returned", s)
		return false
	}

	// Determine if ModSecurity is enabled globally.
	if cfg.EnableModsecurity {
		return true
	}

	// If ModSecurity is not enabled globally, check if any location has it enabled via annotation.
	for _, server := range servers {
		for _, location := range server.Locations {
			if location.ModSecurity.Enable {
				return true
			}
		}
	}

	// Not enabled globally nor via annotation on a location, no need to load the module.
	return false
}

func buildHTTPListener(t interface{}, s interface{}) string {
	var out []string

	tc, ok := t.(config.TemplateConfig)
	if !ok {
		klog.Errorf("expected a 'config.TemplateConfig' type but %T was returned", t)
		return ""
	}

	hostname, ok := s.(string)
	if !ok {
		klog.Errorf("expected a 'string' type but %T was returned", s)
		return ""
	}

	addrV4 := []string{""}
	if len(tc.Cfg.BindAddressIpv4) > 0 {
		addrV4 = tc.Cfg.BindAddressIpv4
	}

	co := commonListenOptions(tc, hostname)

	out = append(out, httpListener(addrV4, co, tc)...)

	if !tc.IsIPV6Enabled {
		return strings.Join(out, "\n")
	}

	addrV6 := []string{"[::]"}
	if len(tc.Cfg.BindAddressIpv6) > 0 {
		addrV6 = tc.Cfg.BindAddressIpv6
	}

	out = append(out, httpListener(addrV6, co, tc)...)

	return strings.Join(out, "\n")
}

func buildHTTPSListener(t interface{}, s interface{}) string {
	var out []string

	tc, ok := t.(config.TemplateConfig)
	if !ok {
		klog.Errorf("expected a 'config.TemplateConfig' type but %T was returned", t)
		return ""
	}

	hostname, ok := s.(string)
	if !ok {
		klog.Errorf("expected a 'string' type but %T was returned", s)
		return ""
	}

	co := commonListenOptions(tc, hostname)

	addrV4 := []string{""}
	if len(tc.Cfg.BindAddressIpv4) > 0 {
		addrV4 = tc.Cfg.BindAddressIpv4
	}

	out = append(out, httpsListener(addrV4, co, tc)...)

	if !tc.IsIPV6Enabled {
		return strings.Join(out, "\n")
	}

	addrV6 := []string{"[::]"}
	if len(tc.Cfg.BindAddressIpv6) > 0 {
		addrV6 = tc.Cfg.BindAddressIpv6
	}

	out = append(out, httpsListener(addrV6, co, tc)...)

	return strings.Join(out, "\n")
}

func commonListenOptions(template config.TemplateConfig, hostname string) string {
	var out []string

	if template.Cfg.UseProxyProtocol {
		out = append(out, "proxy_protocol")
	}

	if hostname != "_" {
		return strings.Join(out, " ")
	}

	// setup options that are valid only once per port

	out = append(out, "default_server")

	if template.Cfg.ReusePort {
		out = append(out, "reuseport")
	}

	out = append(out, fmt.Sprintf("backlog=%v", template.BacklogSize))

	return strings.Join(out, " ")
}

func httpListener(addresses []string, co string, tc config.TemplateConfig) []string {
	out := make([]string, 0)
	for _, address := range addresses {
		l := make([]string, 0)
		l = append(l, "listen")

		if address == "" {
			l = append(l, fmt.Sprintf("%v", tc.ListenPorts.HTTP))
		} else {
			l = append(l, fmt.Sprintf("%v:%v", address, tc.ListenPorts.HTTP))
		}

		l = append(l, co)
		l = append(l, ";")
		out = append(out, strings.Join(l, " "))
	}

	return out
}

func httpsListener(addresses []string, co string, tc config.TemplateConfig) []string {
	out := make([]string, 0)
	for _, address := range addresses {
		l := make([]string, 0)
		l = append(l, "listen")

		if tc.IsSSLPassthroughEnabled {
			if address == "" {
				l = append(l, fmt.Sprintf("%v", tc.ListenPorts.SSLProxy))
			} else {
				l = append(l, fmt.Sprintf("%v:%v", address, tc.ListenPorts.SSLProxy))
			}

			l = append(l, "proxy_protocol")
		} else {
			if address == "" {
				l = append(l, fmt.Sprintf("%v", tc.ListenPorts.HTTPS))
			} else {
				l = append(l, fmt.Sprintf("%v:%v", address, tc.ListenPorts.HTTPS))
			}

			if tc.Cfg.UseProxyProtocol {
				l = append(l, "proxy_protocol")
			}
		}

		l = append(l, co)
		l = append(l, "ssl")

		if tc.Cfg.UseHTTP2 {
			l = append(l, "http2")
		}

		l = append(l, ";")
		out = append(out, strings.Join(l, " "))
	}

	return out
}
