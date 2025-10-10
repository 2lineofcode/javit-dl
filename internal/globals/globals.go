package globals

import (
	"net/http"
	"time"
)

var (
	Client = &http.Client{Timeout: 30 * time.Second}
)

const (
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)
