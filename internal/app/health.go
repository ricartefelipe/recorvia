package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Healthcheck consulta /livez do próprio processo (usado pelo healthcheck do
// container, que não tem shell nem curl).
func Healthcheck(addr, def string) error {
	if addr == "" {
		addr = def
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/livez", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	if err := resp.Body.Close(); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New("processo não saudável")
	}
	return nil
}
