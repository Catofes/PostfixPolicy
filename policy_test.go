package main

import (
	"testing"
	"encoding/json"
	"io/ioutil"
)

func TestMainConfig_Init(t *testing.T) {
	config := MainConfig{
		PgURL:    "postgres://moemail@10.2.255.1:5433/moemail?sslmode=disable",
		UnixPath: "/tmp/PostfixPolicy",
	}
	d, _ := json.Marshal(config)
	_ = ioutil.WriteFile("/tmp/PostfixPolicy.json", d, 0644)
	(&MainConfig{}).Init("/tmp/PostfixPolicy.json")
}

func TestMainPolicy_Init(t *testing.T) {

}
