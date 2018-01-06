package policy

import (
	"testing"
	"encoding/json"
	"io/ioutil"
)

func TestMainConfig_Init(t *testing.T) {
	config := MainConfig{
		PgURL:         "postgres://moemail@10.2.255.1:5433/moemail?sslmode=disable",
		ListenAddress: "127.0.0.1:2555",
	}
	d, _ := json.Marshal(config)
	_ = ioutil.WriteFile("/tmp/PostfixPolicy.json", d, 0644)
	(&MainConfig{}).Init("/tmp/PostfixPolicy.json")
}
