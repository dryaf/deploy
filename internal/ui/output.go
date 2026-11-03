package ui

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
	"text/template"
)

const (
	Reset  = "\033[0m"
	Red    = "\033[31m"
	Green  = "\033[32m"
	Yellow = "\033[33m"
	Blue   = "\033[34m"
	Gray   = "\033[37m"
)

func LogFatal(f string, a ...any) { fmt.Printf(Red+"[FATAL] "+Reset+f+"\n", a...); os.Exit(1) }

func LogInfo(f string, a ...any) { fmt.Printf(Blue+"[INFO] "+Reset+f+"\n", a...) }

func LogSuccess(f string, a ...any) { fmt.Printf(Green+"[DONE] "+Reset+f+"\n", a...) }

func LogWarn(f string, a ...any) { fmt.Printf(Yellow+"[WARN] "+Reset+f+"\n", a...) }

func LogError(f string, a ...any) { fmt.Printf(Red+"[ERR] "+Reset+f+"\n", a...) }

func LogDebug(f string, a ...any) {
	if Verbose {
		fmt.Printf(Gray+f+Reset+"\n", a...)
	}
}

func Confirm(prompt string) bool {
	if DryRun {
		return true
	}
	fmt.Printf("%s [y/N]: ", prompt)
	r := bufio.NewReader(os.Stdin)
	res, _ := r.ReadString('\n')
	return strings.ToLower(strings.TrimSpace(res)) == "y"
}

func Prompt(label string) string {
	fmt.Printf("%s: ", label)
	r := bufio.NewReader(os.Stdin)
	res, _ := r.ReadString('\n')
	return strings.TrimSpace(res)
}

func GenFile(path string, tmplStr string, data any) error {
	if DryRun {
		return nil
	}
	t, err := template.New("t").Parse(tmplStr)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return err
	}
	return os.WriteFile(path, b.Bytes(), 0600)
}
