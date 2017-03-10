/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"bufio"
	"github.com/golang/glog"
	"github.com/mitchellh/mapstructure"
	"k8s.io/ingress/core/pkg/ingress"
	"k8s.io/ingress/core/pkg/ingress/annotations/rewrite"
	"k8s.io/ingress/core/pkg/ingress/defaults"
	"os"
	"strings"
)

type (
	configuration struct {
		Userlists           map[string]userlist
		Backends            []*ingress.Backend
		DefaultServer       *haproxyServer
		HTTPServers         []*haproxyServer
		HTTPSServers        []*haproxyServer
		TCPEndpoints        []ingress.L4Service
		UDPEndpoints        []ingress.L4Service
		PassthroughBackends []*ingress.SSLPassthroughBackend
		Syslog              string `json:"syslog-endpoint"`
	}
	userlist struct {
		ListName string
		Realm    string
		Users    []authUser
	}
	authUser struct {
		Username  string
		Password  string
		Encrypted bool
	}
	// haproxyServer and haproxyLocation build some missing pieces
	// from ingress.Server used by HAProxy
	haproxyServer struct {
		IsDefaultServer bool               `json:"isDefaultServer"`
		Hostname        string             `json:"hostname"`
		SSLCertificate  string             `json:"sslCertificate"`
		SSLPemChecksum  string             `json:"sslPemChecksum"`
		RootLocation    *haproxyLocation   `json:"defaultLocation"`
		Locations       []*haproxyLocation `json:"locations,omitempty"`
		SSLRedirect     bool               `json:"sslRedirect"`
	}
	haproxyLocation struct {
		IsRootLocation bool             `json:"isDefaultLocation"`
		Path           string           `json:"path"`
		Backend        string           `json:"backend"`
		Redirect       rewrite.Redirect `json:"redirect,omitempty"`
		Userlist       userlist         `json:"userlist,omitempty"`
		HAMatchPath    string           `json:"haMatchPath"`
		HAWhitelist    string           `json:"whitelist,omitempty"`
	}
)

func mergeMap(data map[string]string, resultTo interface{}) error {
	if data != nil {
		decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
			WeaklyTypedInput: true,
			Result:           resultTo,
			TagName:          "json",
		})
		if err != nil {
			glog.Warningf("error configuring decoder: %v", err)
		} else {
			if err = decoder.Decode(data); err != nil {
				glog.Warningf("error decoding config: %v", err)
			}
		}
		return err
	}
	return nil
}

func newConfig(cfg *ingress.Configuration, data map[string]string) *configuration {
	userlists := newUserlists(cfg.Servers)
	haHTTPServers, haHTTPSServers, haDefaultServer := newHAProxyServers(userlists, cfg.Servers)
	conf := configuration{
		Userlists:           userlists,
		Backends:            cfg.Backends,
		HTTPServers:         haHTTPServers,
		HTTPSServers:        haHTTPSServers,
		DefaultServer:       haDefaultServer,
		TCPEndpoints:        cfg.TCPEndpoints,
		UDPEndpoints:        cfg.UDPEndpoints,
		PassthroughBackends: cfg.PassthroughBackends,
	}
	mergeMap(data, &conf)
	return &conf
}

func newHAProxyServers(userlists map[string]userlist, servers []*ingress.Server) (haHTTPServers []*haproxyServer, haHTTPSServers []*haproxyServer, haDefaultServer *haproxyServer) {
	haHTTPServers = make([]*haproxyServer, 0, len(servers))
	haHTTPSServers = make([]*haproxyServer, 0, len(servers))
	for _, server := range servers {
		haLocations, haRootLocation := newHAProxyLocations(userlists, server)
		haServer := haproxyServer{
			// Ingress uses `_` hostname as default server
			IsDefaultServer: server.Hostname == "_",
			Hostname:        server.Hostname,
			SSLCertificate:  server.SSLCertificate,
			SSLPemChecksum:  server.SSLPemChecksum,
			RootLocation:    haRootLocation,
			Locations:       haLocations,
			SSLRedirect:     serverSSLRedirect(server),
		}
		if haServer.IsDefaultServer {
			haDefaultServer = &haServer
		} else if haServer.SSLCertificate == "" {
			haHTTPServers = append(haHTTPServers, &haServer)
		} else {
			haHTTPSServers = append(haHTTPSServers, &haServer)
			if !haServer.SSLRedirect {
				haHTTPServers = append(haHTTPServers, &haServer)
			}
		}
	}
	return
}

func newHAProxyLocations(userlists map[string]userlist, server *ingress.Server) (haLocations []*haproxyLocation, haRootLocation *haproxyLocation) {
	locations := server.Locations
	haLocations = make([]*haproxyLocation, len(locations))
	otherPaths := ""
	for i, location := range locations {
		haWhitelist := ""
		for _, cidr := range location.Whitelist.CIDR {
			haWhitelist = haWhitelist + " " + cidr
		}
		users, ok := userlists[location.BasicDigestAuth.File]
		if !ok {
			users = userlist{}
		}
		haLocation := haproxyLocation{
			IsRootLocation: location.Path == "/",
			Path:           location.Path,
			Backend:        location.Backend,
			Redirect:       location.Redirect,
			Userlist:       users,
			HAWhitelist:    haWhitelist,
		}
		// RootLocation `/` means "any other URL" on Ingress.
		// HAMatchPath build this strategy on HAProxy.
		if haLocation.IsRootLocation {
			haRootLocation = &haLocation
		} else {
			otherPaths = otherPaths + " " + location.Path
			haLocation.HAMatchPath = " { path_beg " + haLocation.Path + " }"
		}
		haLocations[i] = &haLocation
	}
	if haRootLocation != nil && otherPaths != "" {
		haRootLocation.HAMatchPath = " !{ path_beg" + otherPaths + " }"
	}
	return
}

// This could be improved creating a list of auth secrets (or even configMaps)
// on Ingress and saving usr(s)/pwd in auth.BasicDigest struct
func newUserlists(servers []*ingress.Server) map[string]userlist {
	userlists := map[string]userlist{}
	for _, server := range servers {
		for _, location := range server.Locations {
			fileName := location.BasicDigestAuth.File
			authType := location.BasicDigestAuth.Type
			if fileName != "" && authType != "digest" {
				_, ok := userlists[fileName]
				if !ok {
					slashPos := strings.LastIndex(fileName, "/")
					dotPos := strings.LastIndex(fileName, ".")
					listName := fileName[slashPos+1 : dotPos]
					users, err := readUsers(fileName, listName)
					if err != nil {
						glog.Errorf("Unexpected error reading %v: %v", listName, err)
						break
					}
					userlists[fileName] = userlist{
						ListName: listName,
						Realm:    location.BasicDigestAuth.Realm,
						Users:    users,
					}
				}
			}
		}
	}
	return userlists
}

func readUsers(fileName string, listName string) ([]authUser, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(file)
	users := []authUser{}
	for scanner.Scan() {
		line := scanner.Text()
		sep := strings.Index(line, ":")
		if sep == -1 {
			glog.Warningf("Missing ':' on userlist '%v'", listName)
			break
		}
		userName := line[0:sep]
		if userName == "" {
			glog.Warningf("Missing username on userlist '%v'", listName)
			break
		}
		if sep == len(line)-1 || line[sep:] == "::" {
			glog.Warningf("Missing '%v' password on userlist '%v'", userName, listName)
			break
		}
		user := authUser{}
		// if usr::pwd
		if string(line[sep+1]) == ":" {
			user = authUser{
				Username:  userName,
				Password:  line[sep+2:],
				Encrypted: false,
			}
		} else {
			user = authUser{
				Username:  userName,
				Password:  line[sep+1:],
				Encrypted: true,
			}
		}
		users = append(users, user)
	}
	return users, nil
}

func serverSSLRedirect(server *ingress.Server) bool {
	for _, location := range server.Locations {
		if !location.Redirect.SSLRedirect {
			return false
		}
	}
	return true
}

func newDefaultConfig() defaults.Backend {
	return defaults.Backend{
		SSLRedirect: true,
	}
}
