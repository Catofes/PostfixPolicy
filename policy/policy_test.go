package policy

import (
	"context"
	"log"
	"testing"

	"github.com/flashmob/go-guerrilla/mail"
)

func TestMainPolicy_getDestination(t *testing.T) {
	mainConfig := MainConfig{
		PgURL:       "postgres://moemail@10.2.255.1:5433/moemail?sslmode=disable",
		QueuePath:   ".",
		ServerMark:  "TEST",
		RegionMark:  "3CN",
		DefaultMark: "0default",
		SqlTimeout:  3}
	mainPolicy := MainPolicy{MainConfig: mainConfig}
	mainPolicy.Init(context.Background())
	dest, _ := mainPolicy.getDestination(
		mail.Address{User: "test", Host: "catofes.com"},
		mail.Address{User: "k", Host: "gmail.com"})
	log.Println(dest.Scheme)
}
