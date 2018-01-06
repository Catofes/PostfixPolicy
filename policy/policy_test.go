package policy

import (
	"testing"
	"github.com/flashmob/go-guerrilla/mail"
	"context"
	"log"
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
		mail.Address{"test", "catofes.com"},
		mail.Address{"k", "gmail.com"})
	log.Println(dest.Scheme)
}
