package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

func TestMediaPagination(t *testing.T) {
	if os.Getenv("JAVIT_DL_BROWSER_TEST") == "" {
		t.Skip("set JAVIT_DL_BROWSER_TEST=1 to run Chrome GUI checks")
	}
	if _, err := os.Stat("../../frontend/dist/index.html"); err != nil {
		t.Skip("build the frontend before running GUI checks")
	}
	choices := make([]Choice, 401)
	for i := range choices {
		choices[i] = Choice{Index: i + 1, Label: "FILE", URL: fmt.Sprintf("https://example.com/%d.mp4", i+1)}
	}
	state, err := json.Marshal(State{Active: true, Status: "Running", ChoiceKind: "media", ChoiceID: 7, Choices: choices})
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`window.go={gui:{App:{Defaults:async()=>({}),GetState:async()=> (%s),Select:async(id,index)=>{window.selected=[id,index]}}}}`, state)
	server := httptest.NewServer(http.FileServer(http.Dir("../../frontend/dist")))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	alloc, stop := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer stop()
	browser, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
	var count int
	var selected []int
	err = chromedp.Run(browser,
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
			return err
		}),
		chromedp.Navigate(server.URL),
		chromedp.WaitVisible("nav", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('.choice').length`, &count),
		chromedp.Click("nav button:last-child", chromedp.ByQuery),
		chromedp.Poll(`document.querySelector('.choice-url').textContent.endsWith('/201.mp4')`, nil),
		chromedp.Click("nav button:last-child", chromedp.ByQuery),
		chromedp.Poll(`document.querySelectorAll('.choice').length === 1 && document.querySelector('.choice-url').textContent.endsWith('/401.mp4')`, nil),
		chromedp.Click("nav button:first-child", chromedp.ByQuery),
		chromedp.Poll(`document.querySelector('.choice-url').textContent.endsWith('/201.mp4')`, nil),
		chromedp.Click("nav button:first-child", chromedp.ByQuery),
		chromedp.Poll(`document.querySelector('.choice-url').textContent.endsWith('/1.mp4')`, nil),
		chromedp.Click(".choice", chromedp.ByQuery),
		chromedp.Poll(`window.selected?.[1] === 1`, nil),
		chromedp.Evaluate(`window.selected`, &selected),
	)
	if err != nil || count != 200 || len(selected) != 2 || selected[0] != 7 || selected[1] != 1 {
		t.Fatalf("pagination/selection: %v count=%d selected=%v", err, count, selected)
	}
}
