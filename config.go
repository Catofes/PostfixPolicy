package main

import (
	"io/ioutil"
	"log"
	"encoding/json"
)

type MainConfig struct {
	UnixPath string
	PgURL    string
}

func (s *MainConfig) Init(path string) *MainConfig {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatal("[Fatal] Can not read config file.")
	}
	if err := json.Unmarshal(data, s); err != nil {
		log.Fatal("[Fatal] Can not parse config file.")
	}
	return s
}
