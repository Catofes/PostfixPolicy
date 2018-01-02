package main

import (
	"net"
	"log"
	"context"
	"database/sql"
	"bytes"
	"io"
)

type MainPolicy struct {
	MainConfig
	ln  net.Listener
	ctx context.Context
	db  *sql.DB
}

func (s *MainPolicy) Init(ctx context.Context) *MainPolicy {
	var err error
	s.ln, err = net.Listen("unix", s.UnixPath)
	s.ctx = ctx
	if err != nil {
		log.Fatal(err)
	}
	s.db, err = sql.Open("postgres", s.PgURL)
	if err != nil {
		log.Fatal(err)
	}
	return s
}

func (s *MainPolicy) loop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go s.handler(conn)
	}
}

func (s *MainPolicy) handler(conn net.Conn) {
	defer conn.Close()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, conn)
	if err != nil {
		log.Println(err.Error())
		return
	}
	if n == 0 {
		log.Println("Empty connection.")
		return
	}
	data, err := buf.ReadString('\n')
	log.Println(data)
}

func (s *MainPolicy) readInfo(data []byte) (map[string]string, error) {
	result := make(map[string]string)
	log.Println(data)
	return result, nil
}

func (s *MainPolicy) Run() {
	s.loop()
}
