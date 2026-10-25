package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/victorarias/brout/internal/config"
	"github.com/victorarias/brout/internal/requestlog"
)

// logs lists the newest entries of the request log.
func logs(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath(), "path to config.toml")
	limit := fs.Int("limit", 20, "how many entries to list, newest first")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("logs takes no arguments; to see one entry, run brout logs show <id>")
	}
	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}
	entries, err := requestlog.Latest(cfg.Log.Dir, *limit, stderr)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintf(stdout, "no requests logged in %s\n", cfg.Log.Dir)
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME (UTC)\tMODEL\tSTATUS\tDURATION\tQUESTIONS")
	for _, e := range entries {
		model := "-"
		if e.Model != nil {
			model = *e.Model
		}
		questions := strings.Join(questionNames(e.Request), ", ")
		if e.ReplayOf != nil {
			questions += " (replay of " + *e.ReplayOf + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%dms\t%s\n", e.ID, e.Time.UTC().Format("2006-01-02 15:04:05"), model, e.Status, e.DurationMS, questions)
	}
	return tw.Flush()
}

// logsShow prints one entry as indented JSON.
func logsShow(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("logs show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath(), "path to config.toml")
	ids, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(ids) != 1 {
		return errors.New("usage: brout logs show <id>")
	}
	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}
	e, err := requestlog.Find(cfg.Log.Dir, ids[0], stderr)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	json.Indent(&out, e.Raw, "", "  ")
	out.WriteByte('\n')
	_, err = stdout.Write(out.Bytes())
	return err
}

// replay sends a logged request through the running server, which routes and
// logs it like any other request (D10), and compares the answers.
func replay(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath(), "path to config.toml")
	model := fs.String("model", "", "replay with this model id instead of the logged request's model")
	url := fs.String("url", "", "the running brout (default: the config's listen)")
	ids, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(ids) != 1 {
		return errors.New("usage: brout replay <id> [--model <model id>]")
	}
	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}
	if *url == "" {
		*url = "http://" + cfg.Listen
	}
	e, err := requestlog.Find(cfg.Log.Dir, ids[0], stderr)
	if err != nil {
		return err
	}
	body, err := replayBody(e, *model)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *url+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Brout-Replay-Of", e.ID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("brout replay sends the request through the running brout serve at %s, which did not answer: %w", *url, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "replayed %s as %s\n\n", e.ID, resp.Header.Get("Brout-Request-Id"))
	before, after := outcomeOf(e.Status, e.Response), outcomeOf(resp.StatusCode, answer)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "QUESTION\tBEFORE: %s\tAFTER: %s\n", before.title(), after.title())
	for _, name := range questionNames(e.Request) {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, before.cell(name), after.cell(name))
	}
	tw.Flush()
	for _, o := range []struct {
		when string
		outcome
	}{{"before", before}, {"after", after}} {
		if o.Detail != nil {
			fmt.Fprintf(stdout, "\n%s: %s\n", o.when, o.Detail.Message)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the replay answered %d", resp.StatusCode)
	}
	return nil
}

// replayBody is the logged request, with its model replaced when model is set.
func replayBody(e requestlog.Entry, model string) ([]byte, error) {
	if string(e.Request) == "null" {
		// brout could not read the body, so there is nothing to send.
		return nil, fmt.Errorf("request %s has no logged body to replay", e.ID)
	}
	var text string
	if json.Unmarshal(e.Request, &text) == nil {
		// brout logs a body that is not JSON as a string.
		if model != "" {
			return nil, fmt.Errorf("request %s is not JSON, so --model cannot replace its model", e.ID)
		}
		return []byte(text), nil
	}
	if model == "" {
		return e.Request, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(e.Request, &top); err != nil {
		return nil, fmt.Errorf("request %s is not a JSON object, so --model cannot replace its model", e.ID)
	}
	top["model"], _ = json.Marshal(model)
	return json.Marshal(top)
}

// questionNames returns the question names of a logged request, sorted.
func questionNames(request json.RawMessage) []string {
	var r struct{ Questions map[string]json.RawMessage }
	json.Unmarshal(request, &r)
	names := make([]string, 0, len(r.Questions))
	for name := range r.Questions {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// outcome is a response body brout answered with: an answer or an error.
type outcome struct {
	Status  int `json:"-"`
	Model   string
	Answers map[string]struct {
		Type                    string
		Noul, Score, Confidence *float64
		Choice                  *string
	}
	Detail *struct {
		ErrorType string `json:"error_type"`
		Message   string
	}
}

func outcomeOf(status int, body []byte) outcome {
	o := outcome{Status: status}
	json.Unmarshal(body, &o)
	return o
}

func (o outcome) title() string {
	if o.Status == http.StatusOK {
		return o.Model
	}
	if o.Detail != nil {
		return fmt.Sprintf("%d %s", o.Status, o.Detail.ErrorType)
	}
	return fmt.Sprint(o.Status)
}

func (o outcome) cell(name string) string {
	a, ok := o.Answers[name]
	switch {
	case !ok:
		return "-"
	case a.Type == "noul" && a.Noul != nil:
		return fmt.Sprintf("noul %.3f", *a.Noul)
	case a.Type == "choice" && a.Choice != nil:
		if a.Confidence == nil {
			return "choice " + *a.Choice
		}
		return fmt.Sprintf("choice %s (confidence %.3f)", *a.Choice, *a.Confidence)
	case a.Type == "score" && a.Score != nil:
		return fmt.Sprintf("score %.3f", *a.Score)
	}
	return a.Type
}

// parseArgs parses flags that come before or after the positional arguments,
// so brout replay <id> --model m works as well as brout replay --model m <id>.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}
