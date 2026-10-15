// Package jev implements bounded, typed decisions. Callers own decision policies.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxRequestBytes = 64000 // Local JSON bytes, not the provider token limit.

var pinnedModel = regexp.MustCompile(`^(typesafe/)?jev-[0-9]+\.[0-9]+(\.[0-9]+|-[0-9]{8})$`)

type Question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

func Noul(instructions, yes, no string) Question {
	return Question{Type: "noul", Instructions: instructions, Criteria: map[string]string{"true": yes, "false": no}}
}

type Usage struct {
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	EstimatedCostUSD float64 `json:"estimatedCostUsd"`
	Attempts         int     `json:"attempts"`
	Unknown          bool    `json:"unknown"` // Some dispatched attempts had no usable usage.
}

func (u *Usage) Add(v Usage) {
	u.InputTokens += v.InputTokens
	u.OutputTokens += v.OutputTokens
	u.EstimatedCostUSD += v.EstimatedCostUSD
	u.Attempts += v.Attempts
	u.Unknown = u.Unknown || v.Unknown
}

type Result struct {
	Model         string             `json:"model"`
	Probabilities map[string]float64 `json:"probabilities"`
	Usage         Usage              `json:"usage"`
}
type Decider interface {
	Decide(context.Context, any, map[string]Question) (Result, error)
}
type Options struct {
	Endpoint, APIKey, Model                               string
	Timeout                                               time.Duration
	Concurrency, Retries                                  int
	MaxCostUSD, InputCostPerMillion, OutputCostPerMillion float64
}
type Client struct {
	opts  Options
	http  *http.Client
	slots chan struct{}
}

func New(opts Options) (*Client, error) {
	u, err := url.Parse(opts.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid Jev endpoint")
	}
	if strings.TrimSpace(opts.APIKey) == "" || !pinnedModel.MatchString(opts.Model) {
		return nil, errors.New("Jev requires a key and pinned model")
	}
	if opts.Timeout <= 0 || opts.Concurrency < 1 || opts.Concurrency > 16 || opts.Retries < 0 || opts.Retries > 2 || !positive(opts.MaxCostUSD) || !positive(opts.InputCostPerMillion) || !finite(opts.OutputCostPerMillion) || opts.OutputCostPerMillion < 0 {
		return nil, errors.New("invalid Jev limits or prices")
	}
	return &Client{opts: opts, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, slots: make(chan struct{}, opts.Concurrency)}, nil
}
func finite(v float64) bool     { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func positive(v float64) bool   { return finite(v) && v > 0 }
func (c *Client) Model() string { return c.opts.Model }

// Decide returns accumulated usage even on error. Reservations are per call,
// never lifetime counters. Unknown billed usage is conservatively estimated.
func (c *Client) Decide(parent context.Context, state any, questions map[string]Question) (Result, error) {
	var out Result
	if len(questions) == 0 || len(questions) > 128 {
		return out, errors.New("invalid Jev question count")
	}
	for id, q := range questions {
		if id == "" || q.Type != "noul" || strings.TrimSpace(q.Instructions) == "" {
			return out, errors.New("invalid Jev question")
		}
	}
	body, err := json.Marshal(struct {
		Model     string              `json:"model"`
		State     any                 `json:"state"`
		Questions map[string]Question `json:"questions"`
	}{c.opts.Model, state, questions})
	if err != nil || len(body) > MaxRequestBytes {
		return out, errors.New("Jev payload exceeds local byte limit or is invalid")
	}
	ctx, cancel := context.WithTimeout(parent, c.opts.Timeout)
	defer cancel()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return out, ctx.Err()
	}
	// Bytes plus overhead is a conservative estimate, not a billing guarantee.
	reserve := (float64(len(body)+4096)*c.opts.InputCostPerMillion + float64(128*len(questions))*c.opts.OutputCostPerMillion) / 1e6
	for attempt := 0; attempt <= c.opts.Retries; attempt++ {
		if out.Usage.EstimatedCostUSD+reserve > c.opts.MaxCostUSD {
			return out, errors.New("Jev request cost budget exceeded")
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.Endpoint, bytes.NewReader(body))
		if e != nil {
			return out, errors.New("invalid Jev request")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.opts.APIKey)
		out.Usage.Attempts++
		resp, e := c.http.Do(req)
		if e != nil {
			out.Usage.Unknown = true
			out.Usage.EstimatedCostUSD += reserve
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			return out, errors.New("Jev transport failure")
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		resp.Body.Close()
		if readErr != nil || len(data) > 1<<20 {
			out.Usage.Unknown = true
			out.Usage.EstimatedCostUSD += reserve
			return out, errors.New("invalid Jev response body")
		}
		var wire struct {
			Model   string `json:"model"`
			Answers map[string]struct {
				Type string   `json:"type"`
				Noul *float64 `json:"noul"`
			} `json:"answers"`
			Usage *struct {
				Input  *int64   `json:"input_tokens"`
				Output *int64   `json:"output_tokens"`
				Cost   *float64 `json:"cost"`
			} `json:"usage"`
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		parseErr := dec.Decode(&wire)
		var tail any
		if parseErr == nil && dec.Decode(&tail) != io.EOF {
			parseErr = errors.New("trailing JSON")
		}
		validUsage := parseErr == nil && wire.Usage != nil && wire.Usage.Input != nil && wire.Usage.Output != nil && *wire.Usage.Input >= 0 && *wire.Usage.Output >= 0
		if validUsage {
			cost := (float64(*wire.Usage.Input)*c.opts.InputCostPerMillion + float64(*wire.Usage.Output)*c.opts.OutputCostPerMillion) / 1e6
			if wire.Usage.Cost != nil {
				if !finite(*wire.Usage.Cost) || *wire.Usage.Cost < 0 {
					validUsage = false
				} else {
					cost = *wire.Usage.Cost
				}
			}
			if validUsage {
				out.Usage.InputTokens += *wire.Usage.Input
				out.Usage.OutputTokens += *wire.Usage.Output
				out.Usage.EstimatedCostUSD += cost
			}
		}
		if !validUsage {
			out.Usage.Unknown = true
			out.Usage.EstimatedCostUSD += reserve
		}
		if resp.StatusCode != http.StatusOK {
			retryable := resp.StatusCode == 429 || resp.StatusCode == 503 || resp.StatusCode == 529 || resp.StatusCode == 502 || resp.StatusCode == 504
			if !retryable || attempt == c.opts.Retries {
				return out, fmt.Errorf("Jev HTTP %d", resp.StatusCode)
			}
			wait := time.Duration(100*(1<<attempt)+rand.IntN(100)) * time.Millisecond
			if v := resp.Header.Get("Retry-After"); v != "" {
				if n, e := strconv.ParseFloat(v, 64); e == nil && finite(n) && n >= 0 {
					if n > float64(c.opts.Timeout.Seconds()) {
						return out, errors.New("Jev retry exceeds deadline")
					}
					wait = max(wait, time.Duration(n*float64(time.Second)))
				} else if t, e := http.ParseTime(v); e == nil {
					wait = max(wait, time.Until(t))
				}
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return out, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		out.Model = wire.Model
		if parseErr != nil || !validUsage || wire.Model != c.opts.Model || len(wire.Answers) != len(questions) {
			return out, errors.New("invalid Jev response contract")
		}
		if out.Usage.EstimatedCostUSD > c.opts.MaxCostUSD {
			return out, errors.New("Jev reported cost exceeds request budget")
		}
		out.Probabilities = make(map[string]float64, len(questions))
		for id := range questions {
			a, ok := wire.Answers[id]
			if !ok || a.Type != "noul" || a.Noul == nil || !finite(*a.Noul) || *a.Noul < 0 || *a.Noul > 1 {
				out.Probabilities = nil
				return out, errors.New("invalid Jev answer coverage or probability")
			}
			out.Probabilities[id] = *a.Noul
		}
		return out, nil
	}
	return out, errors.New("Jev attempts exhausted")
}
