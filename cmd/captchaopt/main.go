// captchaopt benchmarks captcha-extract strategies against a live NVIDIA
// predict call. Focus: page-reuse (sticky tab) vs navigate-each.
//
//	go run ./cmd/captchaopt -runs=4
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	glm52 "glm52-nvidia"
)

const playgroundURL = "https://build.nvidia.com/z-ai/glm-5.2/playground"

type mode int

const (
	modeNavEach mode = iota
	modeStickyExec
	modeStickyReset
	modeUserDataNav
)

type variant struct {
	name string
	mode mode
}

type trial struct {
	extractMs int64
	apiMs     int64
	apiOK     bool
	cold      bool
	err       string
}

var blockedAssetPatterns = []*network.BlockPattern{
	{URLPattern: "*://*:*/*.css", Block: true},
	{URLPattern: "*://*:*/*.woff", Block: true},
	{URLPattern: "*://*:*/*.woff2", Block: true},
	{URLPattern: "*://*:*/*.ttf", Block: true},
	{URLPattern: "*://*:*/*.otf", Block: true},
	{URLPattern: "*://*:*/*.eot", Block: true},
	{URLPattern: "*://*:*/*.mp4", Block: true},
	{URLPattern: "*://*:*/*.webm", Block: true},
	{URLPattern: "*://*:*/*.mp3", Block: true},
	{URLPattern: "*://*:*/*.png", Block: true},
	{URLPattern: "*://*:*/*.jpg", Block: true},
	{URLPattern: "*://*:*/*.jpeg", Block: true},
	{URLPattern: "*://*:*/*.gif", Block: true},
	{URLPattern: "*://*:*/*.webp", Block: true},
	{URLPattern: "*://*:*/*.svg", Block: true},
	{URLPattern: "*://*:*/*.ico", Block: true},
}

func main() {
	runs := flag.Int("runs", 4, "trials per variant (sticky: 1st=cold, rest=steady)")
	only := flag.String("only", "", "comma-separated variant names (default: all)")
	skipAPI := flag.Bool("skip-api", false, "only measure extract latency")
	flag.Parse()

	variants := []variant{
		{name: "nav_each", mode: modeNavEach},
		{name: "sticky_exec", mode: modeStickyExec},
		{name: "sticky_reset", mode: modeStickyReset},
		{name: "userdata_nav", mode: modeUserDataNav},
	}
	if *only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			want[strings.TrimSpace(n)] = true
		}
		filtered := variants[:0]
		for _, v := range variants {
			if want[v.name] {
				filtered = append(filtered, v)
			}
		}
		variants = filtered
	}
	if len(variants) == 0 {
		log.Fatal("no variants selected")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	fmt.Printf("captchaopt: runs=%d skip-api=%v variants=%v\n", *runs, *skipAPI, names(variants))

	type summary struct {
		v         variant
		trials    []trial
		medAll    float64
		medSteady float64
		okRate    float64
		medAPIOK  float64
	}
	var summaries []summary

	for _, v := range variants {
		if ctx.Err() != nil {
			break
		}
		fmt.Printf("\n=== variant %s ===\n", v.name)

		sess, err := newSession(ctx, v)
		if err != nil {
			log.Printf("%s: session: %v", v.name, err)
			continue
		}

		var trials []trial
		for i := 0; i < *runs; i++ {
			if ctx.Err() != nil {
				break
			}
			t := sess.trial(ctx, !*skipAPI)
			trials = append(trials, t)
			kind := "steady"
			if t.cold {
				kind = "cold"
			}
			status := "ok"
			if t.err != "" {
				status = t.err
			} else if !*skipAPI && !t.apiOK {
				status = "api-reject"
			}
			fmt.Printf("  trial %d (%s): extract=%dms api=%dms api_ok=%v (%s)\n",
				i+1, kind, t.extractMs, t.apiMs, t.apiOK, status)
			select {
			case <-ctx.Done():
			case <-time.After(1500 * time.Millisecond):
			}
		}
		sess.close()

		all := make([]float64, 0, len(trials))
		steady := make([]float64, 0, len(trials))
		apiOKs := 0
		apiMsOK := make([]float64, 0, len(trials))
		for _, t := range trials {
			if t.err == "" {
				all = append(all, float64(t.extractMs))
				if !t.cold {
					steady = append(steady, float64(t.extractMs))
				}
			}
			if t.apiOK {
				apiOKs++
				apiMsOK = append(apiMsOK, float64(t.apiMs))
			}
		}
		s := summary{v: v, trials: trials}
		s.medAll = median(all)
		s.medSteady = median(steady)
		if len(trials) > 0 {
			s.okRate = float64(apiOKs) / float64(len(trials))
		}
		s.medAPIOK = median(apiMsOK)
		summaries = append(summaries, s)
	}

	fmt.Println("\n=== summary ===")
	fmt.Printf("%-14s %10s %12s %8s %10s\n", "variant", "med_all", "med_steady", "api_ok%", "med_api")
	bestIdx := -1
	bestScore := math.Inf(1)
	for i, s := range summaries {
		okPct := s.okRate * 100
		fmt.Printf("%-14s %10.0f %12.0f %7.0f%% %10.0f\n",
			s.v.name, s.medAll, s.medSteady, okPct, s.medAPIOK)
		score := s.medSteady
		if score <= 0 {
			score = s.medAll
		}
		if !*skipAPI && s.okRate < 1 {
			continue
		}
		if score > 0 && score < bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	if bestIdx >= 0 {
		s := summaries[bestIdx]
		label := "med_steady"
		val := s.medSteady
		if val <= 0 {
			label = "med_all"
			val = s.medAll
		}
		fmt.Printf("\nbest (100%% api_ok, lowest %s): %s (%.0fms)\n", label, s.v.name, val)
	} else {
		fmt.Println("\nbest: none with full api_ok; inspect trials above")
	}
}

func names(vs []variant) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.name
	}
	return out
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 0 {
		return (cp[mid-1] + cp[mid]) / 2
	}
	return cp[mid]
}

// session keeps one long-lived chromedp browser context alive.
// chromedp allocates Chrome on the first Run and ties the process to that
// context — canceling a timeout used for the first Run kills Chrome
// (CommandContext). So we allocate on browserCtx without a canceling timeout.
type session struct {
	v        variant
	allocCtx context.Context
	cancel   context.CancelFunc // allocator
	browser  context.Context
	bCancel  context.CancelFunc
	userDir  string

	mu     sync.Mutex
	warmed bool
}

func newSession(parent context.Context, v variant) (*session, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-translate", true),
		chromedp.Flag("disable-component-extensions-with-background-pages", true),
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
		chromedp.WindowSize(1280, 900),
	)
	if path := os.Getenv("CHROME_PATH"); path != "" {
		opts = append(opts, chromedp.ExecPath(path))
	}
	if os.Getenv("CHROMEDP_NO_SANDBOX") == "1" {
		opts = append(opts,
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
		)
	}
	if os.Getenv("CHROMEDP_ALLOW_IMAGES") != "1" {
		opts = append(opts, chromedp.Flag("blink-settings", "imagesEnabled=false"))
	}

	s := &session{v: v}
	if v.mode == modeUserDataNav {
		dir, err := os.MkdirTemp("", "captchaopt-userdata-*")
		if err != nil {
			return nil, err
		}
		s.userDir = dir
		opts = append(opts, chromedp.UserDataDir(dir))
	}

	allocCtx, cancel := chromedp.NewExecAllocator(parent, opts...)
	browser, bCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if strings.HasPrefix(msg, "unhandled node event") || strings.HasPrefix(msg, "unhandled page event") {
			return
		}
		log.Printf("ERROR: "+format, args...)
	}))
	// Allocate Chrome on browserCtx itself (do NOT wrap first Run in a
	// canceling timeout — that kills the process via exec.CommandContext).
	if err := chromedp.Run(browser, chromedp.Navigate("about:blank")); err != nil {
		bCancel()
		cancel()
		if s.userDir != "" {
			_ = os.RemoveAll(s.userDir)
		}
		return nil, err
	}
	s.allocCtx = allocCtx
	s.cancel = cancel
	s.browser = browser
	s.bCancel = bCancel
	return s, nil
}

func (s *session) close() {
	if s.bCancel != nil {
		s.bCancel()
	}
	s.cancel()
	if s.userDir != "" {
		_ = os.RemoveAll(s.userDir)
	}
}

func (s *session) trial(parent context.Context, checkAPI bool) trial {
	start := time.Now()
	token, cold, err := s.extract()
	extractMs := time.Since(start).Milliseconds()
	if err != nil {
		return trial{extractMs: extractMs, cold: cold, err: err.Error()}
	}
	if !checkAPI {
		return trial{extractMs: extractMs, cold: cold, apiOK: true}
	}
	apiStart := time.Now()
	apiOK, apiErr := pingAPI(parent, token)
	apiMs := time.Since(apiStart).Milliseconds()
	t := trial{extractMs: extractMs, apiMs: apiMs, apiOK: apiOK, cold: cold}
	if apiErr != nil {
		t.err = apiErr.Error()
	}
	return t
}

func (s *session) extract() (token string, cold bool, err error) {
	switch s.v.mode {
	case modeNavEach, modeUserDataNav:
		return s.extractNavEach()
	case modeStickyExec:
		return s.extractSticky(false)
	case modeStickyReset:
		return s.extractSticky(true)
	default:
		return "", false, fmt.Errorf("unknown mode")
	}
}

func (s *session) extractNavEach() (string, bool, error) {
	// Child tab on the already-allocated browser (cheap).
	tabCtx, tabCancel := chromedp.NewContext(s.browser)
	defer tabCancel()
	runCtx, cancel := context.WithTimeout(tabCtx, 90*time.Second)
	defer cancel()

	token, err := navigateAndExecute(runCtx)
	return token, true, err
}

func (s *session) extractSticky(withReset bool) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	runCtx, cancel := context.WithTimeout(s.browser, 90*time.Second)
	defer cancel()

	if !s.warmed {
		token, err := navigateAndExecute(runCtx)
		if err != nil {
			return "", true, err
		}
		s.warmed = true
		return token, true, nil
	}
	token, err := executeOnly(runCtx, withReset)
	return token, false, err
}

func navigateAndExecute(ctx context.Context) (string, error) {
	var token string
	err := chromedp.Run(ctx,
		network.Enable(),
		network.SetBlockedURLs().WithURLPatterns(blockedAssetPatterns),
		chromedp.Navigate(playgroundURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Evaluate(`Object.defineProperty(navigator, 'webdriver', {get: () => undefined})`, nil),
		chromedp.WaitReady(`[data-hcaptcha-widget-id]`, chromedp.ByQuery),
		waitHCaptchaReady(),
		chromedp.Evaluate(execJS(false), &token),
	)
	if err != nil {
		return "", err
	}
	if token == "" {
		token, err = pollToken(ctx, false)
	}
	if token == "" && err == nil {
		err = fmt.Errorf("empty captcha token")
	}
	return token, err
}

func executeOnly(ctx context.Context, withReset bool) (string, error) {
	var prev, token string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		const el = document.querySelector('[data-hcaptcha-widget-id]');
		return el ? (el.getAttribute('data-hcaptcha-response') || '') : '';
	})()`, &prev)); err != nil {
		return "", err
	}

	err := chromedp.Run(ctx, chromedp.Evaluate(execJS(withReset), &token))
	if err != nil {
		return "", err
	}
	if token == "" || token == prev {
		token, err = pollTokenUntilChange(ctx, prev, withReset)
	}
	if token == "" && err == nil {
		err = fmt.Errorf("empty captcha token on sticky execute")
	}
	if token != "" && token == prev {
		err = fmt.Errorf("sticky execute returned same token (not refreshed)")
	}
	return token, err
}

func execJS(withReset bool) string {
	// Production path (internal/captcha) uses execute({async:true}) only.
	// sticky_reset still exercises reset+sync execute for A/B; sticky_exec
	// matches the Playground hook (async execute + getResponse).
	if !withReset {
		// Align with production: sync fire execute({async:true}); chromedp cannot await.
		return `(() => {
			const el = document.querySelector('[data-hcaptcha-widget-id]');
			if (!el || typeof hcaptcha === 'undefined') return '';
			const id = el.getAttribute('data-hcaptcha-widget-id');
			try { hcaptcha.execute(id, { async: true }); } catch (e) {}
			try {
				const t = typeof hcaptcha.getResponse === 'function' ? hcaptcha.getResponse(id) : '';
				if (typeof t === 'string' && t) return t;
			} catch (e) {}
			return el.getAttribute('data-hcaptcha-response') || '';
		})()`
	}
	return `(() => {
		const el = document.querySelector('[data-hcaptcha-widget-id]');
		if (!el || typeof hcaptcha === 'undefined') return '';
		const id = el.getAttribute('data-hcaptcha-widget-id');
		el.setAttribute('data-hcaptcha-response', '');
		try { hcaptcha.reset(id); } catch (e) {}
		try { hcaptcha.execute(id); } catch (e) {}
		return el.getAttribute('data-hcaptcha-response') || '';
	})()`
}

func waitHCaptchaReady() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			var ready bool
			if err := chromedp.Evaluate(`typeof hcaptcha !== 'undefined'`, &ready).Do(ctx); err != nil {
				return err
			}
			if ready {
				return nil
			}
			if err := chromedp.Sleep(100 * time.Millisecond).Do(ctx); err != nil {
				return err
			}
		}
		return fmt.Errorf("hcaptcha global not ready")
	})
}

func pollToken(ctx context.Context, withReset bool) (string, error) {
	var token string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := chromedp.Run(ctx,
			chromedp.Sleep(200*time.Millisecond),
			chromedp.Evaluate(`(() => {
				const el = document.querySelector('[data-hcaptcha-widget-id]');
				return el ? (el.getAttribute('data-hcaptcha-response') || '') : '';
			})()`, &token),
		); err != nil {
			return "", err
		}
		if token != "" {
			return token, nil
		}
		_ = chromedp.Run(ctx, chromedp.Evaluate(execJS(withReset), nil))
	}
	return "", nil
}

func pollTokenUntilChange(ctx context.Context, prev string, withReset bool) (string, error) {
	var token string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := chromedp.Run(ctx,
			chromedp.Sleep(150*time.Millisecond),
			chromedp.Evaluate(`(() => {
				const el = document.querySelector('[data-hcaptcha-widget-id]');
				return el ? (el.getAttribute('data-hcaptcha-response') || '') : '';
			})()`, &token),
		); err != nil {
			return "", err
		}
		if token != "" && token != prev {
			return token, nil
		}
		_ = chromedp.Run(ctx, chromedp.Evaluate(execJS(withReset), nil))
	}
	return token, nil
}

func pingAPI(ctx context.Context, token string) (bool, error) {
	client := glm52.New(
		glm52.WithCaptchaToken(token),
		glm52.WithThinking(false),
		glm52.WithDefaults(8, 42, 1.0, 1.0),
	)
	apiCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := client.Chat(apiCtx, []glm52.Message{
		{Role: "user", Content: "ping"},
	})
	if err != nil {
		return false, err
	}
	if resp == nil || len(resp.Choices) == 0 {
		return false, fmt.Errorf("empty response")
	}
	return true, nil
}
