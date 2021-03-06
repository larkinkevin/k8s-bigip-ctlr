/*-
 * Copyright (c) 2016,2017, F5 Networks, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR conditionS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package appmanager

import (
	"bytes"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	log "github.com/F5Networks/k8s-bigip-ctlr/pkg/vlogger"

	routeapi "github.com/openshift/origin/pkg/route/api"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
)

const httpRedirectIRuleName = "http_redirect_irule"
const sslPassthroughIRuleName = "openshift_passthrough_irule"

// Internal data group for passthrough routes to map server names to pools.
const passthroughHostsDgName = "ssl_passthrough_servername_dg"

// Internal data group for reencrypt routes.
// FIXME: Only used by the iRule below until reencrypt is supported.
const reencryptHostsDgName = "ssl_reencrypt_servername_dg"

func (r Rules) Len() int           { return len(r) }
func (r Rules) Less(i, j int) bool { return r[i].FullURI < r[j].FullURI }
func (r Rules) Swap(i, j int)      { r[i], r[j] = r[j], r[i] }

type Routes []*routeapi.Route

func createRule(uri, poolName, partition, routeName string) (*Rule, error) {
	_u := "scheme://" + uri
	_u = strings.TrimSuffix(_u, "/")
	u, err := url.Parse(_u)
	if nil != err {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteRune('/')
	b.WriteString(partition)
	b.WriteRune('/')
	b.WriteString(poolName)

	a := action{
		Forward: true,
		Name:    "0",
		Pool:    b.String(),
		Request: true,
	}

	var c []*condition
	if true == strings.HasPrefix(uri, "*.") {
		c = append(c, &condition{
			EndsWith: true,
			Host:     true,
			HTTPHost: true,
			Name:     "0",
			Index:    0,
			Request:  true,
			Values:   []string{strings.TrimPrefix(u.Host, "*")},
		})
	} else if u.Host != "" {
		c = append(c, &condition{
			Equals:   true,
			Host:     true,
			HTTPHost: true,
			Name:     "0",
			Index:    0,
			Request:  true,
			Values:   []string{u.Host},
		})
	}
	if 0 != len(u.EscapedPath()) {
		path := strings.TrimPrefix(u.EscapedPath(), "/")
		segments := strings.Split(path, "/")
		for i, v := range segments {
			c = append(c, &condition{
				Equals:      true,
				HTTPURI:     true,
				PathSegment: true,
				Name:        strconv.Itoa(i + 1),
				Index:       i + 1,
				Request:     true,
				Values:      []string{v},
			})
		}
	}

	rl := Rule{
		Name:       routeName,
		FullURI:    uri,
		Actions:    []*action{&a},
		Conditions: c,
	}

	log.Debugf("Configured rule: %v", rl)
	return &rl, nil
}

func createPolicy(rls Rules, policyName, partition string) *Policy {
	plcy := Policy{
		Controls:  []string{"forwarding"},
		Legacy:    true,
		Name:      policyName,
		Partition: partition,
		Requires:  []string{"http"},
		Rules:     Rules{},
		Strategy:  "/Common/first-match",
	}

	plcy.Rules = rls

	log.Debugf("Configured policy: %v", plcy)
	return &plcy
}

func processIngressRules(
	ing *v1beta1.IngressSpec,
	pools []Pool,
	partition string,
) *Rules {
	var err error
	var uri, poolName string
	var rl *Rule
	rlMap := make(ruleMap)
	wildcards := make(ruleMap)
	for _, rule := range ing.Rules {
		if nil != rule.IngressRuleValue.HTTP {
			for _, path := range rule.IngressRuleValue.HTTP.Paths {
				uri = rule.Host + path.Path
				for _, pool := range pools {
					if path.Backend.ServiceName == pool.ServiceName {
						poolName = pool.Name
					}
				}
				if poolName == "" {
					continue
				}
				// This blank name gets overridden by an ordinal later on
				rl, err = createRule(uri, poolName, partition, "")
				if nil != err {
					log.Warningf("Error configuring rule: %v", err)
					return nil
				}
				if true == strings.HasPrefix(uri, "*.") {
					wildcards[uri] = rl
				} else {
					rlMap[uri] = rl
				}
				poolName = ""
			}
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	sortrules := func(r ruleMap, rls *Rules, ordinal int) {
		for _, v := range r {
			*rls = append(*rls, v)
		}
		sort.Sort(sort.Reverse(*rls))
		for _, v := range *rls {
			v.Ordinal = ordinal
			v.Name = strconv.Itoa(ordinal)
			ordinal++
		}
		wg.Done()
	}
	rls := Rules{}
	go sortrules(rlMap, &rls, 0)

	w := Rules{}
	go sortrules(wildcards, &w, len(rlMap))

	wg.Wait()

	rls = append(rls, w...)
	return &rls
}

func httpRedirectIRule(port int32) string {
	iRuleCode := fmt.Sprintf(`
	when HTTP_REQUEST {
       HTTP::redirect https://[getfield [HTTP::host] ":" 1]:%d[HTTP::uri]
    }`, port)

	return iRuleCode
}

func sslPassthroughIRule() string {
	iRuleCode := `
when CLIENT_ACCEPTED {
	TCP::collect
}

when CLIENT_DATA {
	# Byte 0 is the content type.
	# Bytes 1-2 are the TLS version.
	# Bytes 3-4 are the TLS payload length.
	# Bytes 5-$tls_payload_len are the TLS payload.
	binary scan [TCP::payload] cSS tls_content_type tls_version tls_payload_len

	switch $tls_version {
		"769" -
		"770" -
		"771" {
			# Content type of 22 indicates the TLS payload contains a handshake.
			if { $tls_content_type == 22 } {
				# Byte 5 (the first byte of the handshake) indicates the handshake
				# record type, and a value of 1 signifies that the handshake record is
				# a ClientHello.
				binary scan [TCP::payload] @5c tls_handshake_record_type
				if { $tls_handshake_record_type == 1 } {
					# Bytes 6-8 are the handshake length (which we ignore).
					# Bytes 9-10 are the TLS version (which we ignore).
					# Bytes 11-42 are random data (which we ignore).

					# Byte 43 is the session ID length.  Following this are three
					# variable-length fields which we shall skip over.
					set record_offset 43

					# Skip the session ID.
					binary scan [TCP::payload] @${record_offset}c tls_session_id_len
					incr record_offset [expr {1 + $tls_session_id_len}]

					# Skip the cipher_suites field.
					binary scan [TCP::payload] @${record_offset}S tls_cipher_suites_len
					incr record_offset [expr {2 + $tls_cipher_suites_len}]

					# Skip the compression_methods field.
					binary scan [TCP::payload] @${record_offset}c tls_compression_methods_len
					incr record_offset [expr {1 + $tls_compression_methods_len}]

					# Get the number of extensions, and store the extensions.
					binary scan [TCP::payload] @${record_offset}S tls_extensions_len
					incr record_offset 2
					binary scan [TCP::payload] @${record_offset}a* tls_extensions

					for { set extension_start 0 }
							{ $tls_extensions_len - $extension_start == abs($tls_extensions_len - $extension_start) }
							{ incr extension_start 4 } {
						# Bytes 0-1 of the extension are the extension type.
						# Bytes 2-3 of the extension are the extension length.
						binary scan $tls_extensions @${extension_start}SS extension_type extension_len

						# Extension type 00 is the ServerName extension.
						if { $extension_type == "00" } {
							# Bytes 4-5 of the extension are the SNI length (we ignore this).

							# Byte 6 of the extension is the SNI type.
							set sni_type_offset [expr {$extension_start + 6}]
							binary scan $tls_extensions @${sni_type_offset}S sni_type

							# Type 0 is host_name.
							if { $sni_type == "0" } {
								# Bytes 7-8 of the extension are the SNI data (host_name)
								# length.
								set sni_len_offset [expr {$extension_start + 7}]
								binary scan $tls_extensions @${sni_len_offset}S sni_len

								# Bytes 9-$sni_len are the SNI data (host_name).
								set sni_start [expr {$extension_start + 9}]
								binary scan $tls_extensions @${sni_start}A${sni_len} tls_servername
							}
						}

						incr extension_start $extension_len
					}

					if { [info exists tls_servername] } {
						set servername_lower [string tolower $tls_servername]
						SSL::disable serverside
						if { [class match $servername_lower equals ssl_passthrough_servername_dg] } {
							pool [class match -value $servername_lower equals ssl_passthrough_servername_dg]
							SSL::disable
							HTTP::disable
						}
						elseif { [class match $servername_lower equals ssl_reencrypt_servername_dg] } {
							pool [class match -value $servername_lower equals ssl_reencrypt_servername_dg]
							SSL::enable serverside
						}
					}
				}
			}
		}
	}

	TCP::release
}
`
	return iRuleCode
}

// Update a specific datagroup for passthrough routes, indicating if
// something had changed.
func (appMgr *Manager) updatePassthroughRouteDataGroups(
	partition string,
	poolName string,
	hostName string,
) (bool, error) {

	changed := false
	key := nameRef{
		Name:      passthroughHostsDgName,
		Partition: partition,
	}

	appMgr.intDgMutex.Lock()
	defer appMgr.intDgMutex.Unlock()
	hostDg, found := appMgr.intDgMap[key]
	if false == found {
		return false, fmt.Errorf("Internal Data-group /%s/%s does not exist.",
			partition, passthroughHostsDgName)
	}

	if hostDg.AddOrUpdateRecord(hostName, poolName) {
		changed = true
	}

	return changed, nil
}

// Update a data group map based on a passthrough route object.
func updateDataGroupForPassthroughRoute(
	route *routeapi.Route,
	partition string,
	dgMap InternalDataGroupMap,
) {
	hostName := route.Spec.Host
	poolName := formatRoutePoolName(route)
	updateDataGroup(dgMap, passthroughHostsDgName,
		partition, hostName, poolName)
}

// Update a data group map based on a reencrypt route object.
func updateDataGroupForReencryptRoute(
	route *routeapi.Route,
	partition string,
	dgMap InternalDataGroupMap,
) {
	hostName := route.Spec.Host
	poolName := formatRoutePoolName(route)
	updateDataGroup(dgMap, reencryptHostsDgName,
		partition, hostName, poolName)
}

// Add or update a data group record
func updateDataGroup(
	intDgMap InternalDataGroupMap,
	name string,
	partition string,
	key string,
	value string,
) {
	mapKey := nameRef{
		Name:      name,
		Partition: partition,
	}
	dg, found := intDgMap[mapKey]
	if found {
		dg.AddOrUpdateRecord(key, value)
	} else {
		newDg := InternalDataGroup{
			Name:      name,
			Partition: partition,
		}
		newDg.AddOrUpdateRecord(key, value)
		intDgMap[mapKey] = &newDg
	}
}

// Update the appMgr datagroup cache for passthrough routes, indicating if
// something had changed by updating 'stats', which should rewrite the config.
func (appMgr *Manager) updateRouteDataGroups(
	stats *vsSyncStats,
	dgMap InternalDataGroupMap,
) {
	appMgr.intDgMutex.Lock()
	defer appMgr.intDgMutex.Unlock()

	for _, grp := range dgMap {
		mapKey := nameRef{
			Name:      grp.Name,
			Partition: grp.Partition,
		}
		dg, found := appMgr.intDgMap[mapKey]
		if found {
			if !reflect.DeepEqual(dg.Records, grp.Records) {
				dg.Records = grp.Records
				stats.dgUpdated += 1
			}
		} else {
			appMgr.intDgMap[mapKey] = grp
		}
	}
}

func (slice Routes) Len() int {
	return len(slice)
}

func (slice Routes) Less(i, j int) bool {
	return (slice[i].Spec.Host < slice[j].Spec.Host) ||
		(slice[i].Spec.Host == slice[j].Spec.Host &&
			slice[i].Spec.Path < slice[j].Spec.Path)
}

func (slice Routes) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

func (appInf *appInformer) getOrderedRoutes(namespace string) (Routes, error) {
	routeByIndex, err := appInf.routeInformer.GetIndexer().ByIndex(
		"namespace", namespace)
	var routes Routes
	for _, obj := range routeByIndex {
		route := obj.(*routeapi.Route)
		routes = append(routes, route)
	}
	sort.Sort(routes)
	return routes, err
}
