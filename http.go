// Copyright 2013 Xing Xing <mikespook@gmail.com>.
// All rights reserved.
// Use of this source code is governed by a commercial
// license that can be found in the LICENSE file.

package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mikespook/golib/idgen"
	"github.com/mikespook/golib/iptpool"
	"github.com/mikespook/golib/log"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"path"
)

var (
	ErrAccessDeny       = errors.New("Access Deny")
	ErrMethodNotAllowed = errors.New("Method Not Allowed")
	ErrSyncNeeded       = errors.New("`sync` param needed")
)

type httpServer struct {
	conn       net.Listener
	srv        *http.Server
	iptPool    *iptpool.IptPool
	secret     string
	scriptPath string
	idgen      idgen.IdGen
}

func NewHook(addr, scriptPath, secret string) (srv *httpServer) {
	srv = &httpServer{
		srv:        &http.Server{Addr: addr},
		iptPool:    iptpool.NewIptPool(NewLuaIpt),
		scriptPath: scriptPath,
		secret:     secret,
		idgen:      idgen.NewObjectId(),
	}
	return
}

func (s *httpServer) SetTLS(certFile, keyFile string) (err error) {
	s.srv.TLSConfig = &tls.Config{}
	s.srv.TLSConfig.NextProtos = []string{"http/1.1"}
	s.srv.TLSConfig.Certificates = make([]tls.Certificate, 1)
	s.srv.TLSConfig.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
	return
}

func (s *httpServer) Serve() (err error) {
	s.conn, err = net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return
	}
	if s.srv.TLSConfig != nil {
		s.conn = tls.NewListener(s.conn, s.srv.TLSConfig)
	}
	s.iptPool.OnCreate = func(ipt iptpool.ScriptIpt) error {
		ipt.Init(s.scriptPath)
		ipt.Bind("Call", s.call)
		ipt.Bind("Secret", s.secret)
		return nil
	}
	http.HandleFunc("/", s.handler)
	return s.srv.Serve(s.conn)
}

func (s *httpServer) Close() error {
	errstr := ""
	emap := s.iptPool.Free()
	if n := len(emap); n > 0 {
		for k, err := range emap {
			errstr = fmt.Sprintf("%s[%s]: %s\n", errstr, k, err)
		}
	}
	s.conn.Close()
	if errstr != "" {
		return errors.New(errstr)
	}
	return nil
}

func (s *httpServer) verify(p url.Values) bool {
	if s.secret == "" {
		return true
	}
	return s.secret == p.Get("secret")
}

func (s *httpServer) handler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		fallthrough
	case "POST":
	default:
		log.Errorf("[%s] %s \"%s: %s\"", r.RemoteAddr, r.RequestURI, ErrMethodNotAllowed, r.Method)
		http.Error(w, ErrMethodNotAllowed.Error(), 405)
		return
	}
	u, err := url.Parse(r.RequestURI)
	if err != nil {
		log.Errorf("[%s] %s \"%s\"", r.RemoteAddr, r.RequestURI, err)
		http.Error(w, err.Error(), 500)
		return
	}
	p := u.Query()
	if !s.verify(p) { // verify secret token
		log.Errorf("[%s] %s \"%s\"", r.RemoteAddr, r.RequestURI, ErrAccessDeny)
		http.Error(w, ErrAccessDeny.Error(), 403)
		return
	}
	p.Del("secret")
	params := make(Params)
	params.AddValues(p)
	if r.Method == "POST" {
		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Errorf("[%s] %s \"%s\"", r.RemoteAddr, r.RequestURI, err)
			http.Error(w, err.Error(), 500)
			return
		}
		defer r.Body.Close()
		if err := params.AddJSON(data); err != nil {
			log.Errorf("[%s] %s \"%s\"", r.RemoteAddr, r.RequestURI, err)
			http.Error(w, err.Error(), 500)
			return
		}
	}
	name := path.Base(u.Path)
	id := s.idgen.Id().(string)
	f := func(sync bool) {
		ipt := s.iptPool.Get()
		defer s.iptPool.Put(ipt)
		ipt.Bind("Id", id)
		ipt.Bind("WriteBody", func(str string) (err error) {
			if !sync {
				return ErrSyncNeeded
			}
			_, err = w.Write([]byte(str))
			return
		})
		ipt.Bind("WriteHeader", func(status int) error {
			if !sync {
				return ErrSyncNeeded
			}
			w.WriteHeader(status)
			return nil
		})

		if err := ipt.Exec(name, params); err != nil {
			log.Errorf("[%s] %s \"%s\"", r.RemoteAddr,
				r.RequestURI, err.Error())
			if sync {
				http.Error(w, err.Error(), 500)
			}
			return
		}
		log.Messagef("[%s] %s \"Success\"", r.RemoteAddr,
			r.RequestURI)
	}

	if p.Get("sync") == "true" {
		f(true)
		w.Header().Set("Ghoko-Id", id)
	} else {
		go f(false)
		if _, err := w.Write([]byte(id)); err != nil {
			log.Errorf("[%s] %s %s \"%s\"", r.RemoteAddr,
				r.RequestURI, id, err)
		}
	}
}

func (s *httpServer) call(id, name string, params Params) (err error) {
	ipt := s.iptPool.Get()
	defer s.iptPool.Put(ipt)
	ipt.Bind("Id", id)
	return ipt.Exec(name, params)
}

func (s *httpServer) callRemote(id, name string, params Params) (err error) {
	ipt := s.iptPool.Get()
	defer s.iptPool.Put(ipt)
	ipt.Bind("Id", id)
	return ipt.Exec(name, params)
}

func (s *httpServer) jsonencode(data map[string]interface{}) (jsonBlod string, err error) {
	var b []byte
	if b, err = json.Marshal(data); err == nil {
		jsonBlod = string(b)
	}
	return
}

func (s *httpServer) jsondecode(jsonBlod string) (data map[string]interface{}, err error) {
	err = json.Unmarshal([]byte(jsonBlod), &data)
	return
}
