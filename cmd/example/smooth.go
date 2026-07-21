package main

import (
	"fmt"
	"os"
	"time"
	"unicode/utf8"
)

// writeSmooth prints s rune-by-rune with a fixed delay. delay<=0 prints at once.
func writeSmooth(s string, delay time.Duration) {
	if delay <= 0 || s == "" {
		fmt.Print(s)
		return
	}
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		fmt.Printf("%c", r)
		_ = os.Stdout.Sync()
		s = s[size:]
		time.Sleep(delay)
	}
}
