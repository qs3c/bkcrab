// rag-rerank-bench is a read-only, opt-in experiment runner. It never starts
// workers, migrates the database, or changes production routing.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/rag/chunktext"
	"github.com/qs3c/bkcrab/internal/rag/dialogue"
	"github.com/qs3c/bkcrab/internal/rag/rerank"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

type candidate struct {
	ID         string  `json:"id"`
	SearchText string  `json:"searchText"`
	Hit        rag.Hit `json:"hit"`
}
type sample struct {
	ID                   string          `json:"id"`
	Language             string          `json:"language"`
	Split                string          `json:"split"`
	Query                string          `json:"query"`
	Reference            string          `json:"reference"`
	ReferenceContexts    []string        `json:"referenceContexts"`
	ReferenceDocumentIDs []string        `json:"referenceDocumentIds"`
	RelevantIDs          []string        `json:"relevantIds,omitempty"`
	Group                string          `json:"group"`
	ExpectedAbstention   bool            `json:"expectedAbstention"`
	History              []dialogue.Turn `json:"history,omitempty"`
	Candidates           []candidate     `json:"candidates"`
	SearchTrace          rag.SearchTrace `json:"searchTrace"`
}
type frozen struct {
	Version    int                       `json:"version"`
	Generation string                    `json:"generation"`
	CreatedAt  time.Time                 `json:"createdAt"`
	Profile    config.RAGEvalProfileData `json:"profile"`
	Cases      []sample                  `json:"cases"`
}
type record struct {
	Kind       string           `json:"kind"`
	Manifest   string           `json:"manifest"`
	CaseID     string           `json:"caseId"`
	Arm        string           `json:"arm"`
	Repeat     int              `json:"repeat"`
	Phase      string           `json:"phase"`
	StartedAt  time.Time        `json:"startedAt"`
	DurationMS int64            `json:"durationMs"`
	Status     string           `json:"status"`
	Error      string           `json:"error,omitempty"`
	Scores     []rerank.Result  `json:"scores,omitempty"`
	Calls      []rerank.JevCall `json:"calls,omitempty"`
	Answer     *rag.AnswerTrace `json:"answer,omitempty"`
	ContextIDs []string         `json:"contextIds,omitempty"`
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func key(r record) string {
	return fmt.Sprintf("%s/%s/%s/%d/%s", r.Kind, r.CaseID, r.Arm, r.Repeat, r.Phase)
}
func load(path string) (f frozen, hash string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return f, "", err
	}
	err = json.Unmarshal(b, &f)
	if err != nil {
		return f, "", err
	}
	seen := map[string]bool{}
	for _, s := range f.Cases {
		if s.ID == "" || seen[s.ID] || strings.TrimSpace(s.Query) == "" || len(s.Candidates) == 0 || len(s.Candidates) > 20 {
			return f, "", errors.New("invalid or duplicate case")
		}
		seen[s.ID] = true
		ids := map[string]bool{}
		for _, c := range s.Candidates {
			if c.ID == "" || ids[c.ID] || strings.TrimSpace(c.SearchText) == "" || math.IsNaN(c.Hit.Score) || math.IsInf(c.Hit.Score, 0) {
				return f, "", errors.New("invalid or duplicate candidate")
			}
			ids[c.ID] = true
		}
	}
	return f, digest(b), nil
}
func readRecords(path string) ([]record, error) {
	f, e := os.Open(path)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	var rows []record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 65536), 16<<20)
	for sc.Scan() {
		var r record
		if e = json.Unmarshal(sc.Bytes(), &r); e != nil {
			return nil, e
		}
		rows = append(rows, r)
	}
	return rows, sc.Err()
}
func openOutput(path, hash string) (*os.File, map[string]bool, error) {
	rows, e := readRecords(path)
	if e != nil {
		return nil, nil, e
	}
	done := map[string]bool{}
	for _, r := range rows {
		if r.Manifest != hash {
			return nil, nil, errors.New("output belongs to different experiment configuration")
		}
		if done[key(r)] {
			return nil, nil, errors.New("duplicate result")
		}
		done[key(r)] = true
	}
	f, e := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	return f, done, e
}
func emit(f *os.File, r record) {
	must(json.NewEncoder(f).Encode(r))
	must(f.Sync())
	fmt.Printf("%s %s %s repeat=%d %s %dms\n", r.Kind, r.CaseID, r.Arm, r.Repeat, r.Status, r.DurationMS)
}
func db() (*store.DBStore, *config.EnvConfig) {
	e := config.LoadEnv()
	s, err := store.NewDBStore(e.Storage.Type, e.Storage.DSN)
	must(err)
	return s, e
}
func freeze(ctx context.Context, path, generation, owner, run string) {
	st, e := db()
	defer st.Close()
	cfg := e.RAG
	cfg.ApplyDefaults()
	gen, err := st.GetRAGEvalGeneration(ctx, generation)
	must(err)
	r, err := st.GetRAGEvalRun(ctx, run)
	must(err)
	p, err := st.GetRAGEvalProfile(ctx, r.ProfileID)
	must(err)
	var profile config.RAGEvalProfileData
	must(json.Unmarshal([]byte(p.ProfileJSON), &profile))
	profile.Runtime.TopN = 20
	profile.Runtime.CandidateTopK = 20
	profile.Runtime.MinScore = 0
	profile.RewriteEnabled = false
	profile.HyDEEnabled = false
	profile.RerankerEnabled = false
	v, err := vector.NewMilvus(ctx, cfg.Milvus.Address, cfg.Milvus.Username, cfg.Milvus.Password)
	must(err)
	defer v.Close(ctx)
	svc := rag.New(rag.Deps{Store: st, Vector: v, Cfg: cfg, WorkerMode: rag.WorkerModePaused})
	data := frozen{Version: 1, Generation: generation, CreatedAt: time.Now().UTC(), Profile: profile}
	cursor := ""
	for {
		batch, err := st.ListRAGEvalCases(ctx, gen.DatasetVersionID, cursor, 200)
		must(err)
		if len(batch) == 0 {
			break
		}
		for _, q := range batch {
			s := sample{ID: q.ID, Language: "en", Split: "test", Query: q.UserInput, Reference: q.ReferenceAnswer, ExpectedAbstention: q.ExpectedAbstention}
			must(json.Unmarshal([]byte(q.ReferenceContextsJSON), &s.ReferenceContexts))
			must(json.Unmarshal([]byte(q.ReferenceDocumentIDsJSON), &s.ReferenceDocumentIDs))
			if q.HistoryJSON != "" {
				must(json.Unmarshal([]byte(q.HistoryJSON), &s.History))
			}
			s.Group = strings.Join(s.ReferenceDocumentIDs, "|")
			if s.Group == "" {
				s.Group = s.ID
			}
			hits, trace, err := svc.SearchEvaluationWithOptions(ctx, owner, gen, rag.SearchContext{Query: s.Query, History: s.History}, profile)
			must(err)
			s.SearchTrace = trace
			for _, h := range hits {
				text := h.SearchContent
				if strings.TrimSpace(text) == "" {
					text = chunktext.Search(h.SectionTitle, h.AnswerText())
				}
				s.Candidates = append(s.Candidates, candidate{ID: fmt.Sprintf("%s:%d", h.DocID, h.ChunkIndex), SearchText: text, Hit: h})
			}
			data.Cases = append(data.Cases, s)
			fmt.Printf("freeze %s candidates=%d %dms\n", s.ID, len(hits), trace.DurationMS)
		}
		cursor = batch[len(batch)-1].ID
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	must(err)
	defer f.Close()
	must(json.NewEncoder(f).Encode(data))
	must(f.Sync())
}
func main() {
	mode := flag.String("mode", "rank", "freeze, rank, answer")
	input := flag.String("input", "candidates.json", "frozen cases")
	output := flag.String("output", "ranks.jsonl", "append-only results")
	ranks := flag.String("ranks", "ranks.jsonl", "input ranks for answers")
	generation := flag.String("generation", "reg_13014a14cc8c4bca8989692e69ca10d7", "existing generation")
	owner := flag.String("owner", "u_447a2f8f07032ad989d7", "existing dataset owner")
	run := flag.String("source-run", "rer_8db2c5d1cd82d94d79f58e8f6a29cbb1", "source profile")
	armList := flag.String("arms", "qwen3,jev,rrf", "rank arms")
	batch := flag.Int("jev-batch", 5, "candidates per decision")
	slots := flag.Int("jev-concurrency", 2, "Jev HTTP concurrency")
	model := flag.String("jev-model", "typesafe/jev-1.13", "decision model")
	budget := flag.Float64("jev-budget", 5, "conservative dollars per output file")
	repeats := flag.Int("repeats", 3, "formal rounds")
	warmups := flag.Int("warmups", 5, "warmup cases per arm")
	limit := flag.Int("limit", 0, "case limit")
	split := flag.String("split", "test", "test or calibration")
	answerModel := flag.String("answer-model", "opencode-go/deepseek-v4-flash", "explicit answer binding")
	maxTokens := flag.Int("answer-max-tokens", 4096, "answer output limit")
	answerBudget := flag.Int("answer-token-budget", 2000000, "cumulative answer tokens")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	if *mode == "freeze" {
		freeze(ctx, *output, *generation, *owner, *run)
		return
	}
	data, inputHash, err := load(*input)
	must(err)
	cases := []sample{}
	for _, s := range data.Cases {
		if s.Split == *split {
			cases = append(cases, s)
		}
	}
	if *limit > 0 && len(cases) > *limit {
		cases = cases[:*limit]
	}
	if len(cases) == 0 {
		panic("no cases selected")
	}
	arms := strings.Split(*armList, ",")
	for _, a := range arms {
		if a != "qwen3" && a != "jev" && a != "rrf" {
			panic("invalid arm")
		}
	}
	manifest := map[string]any{"inputHash": inputHash, "mode": *mode, "arms": arms, "batch": *batch, "slots": *slots, "model": *model, "instruction": rerank.JevRelevanceInstruction, "budget": *budget, "repeats": *repeats, "warmups": *warmups, "limit": *limit, "split": *split, "answerModel": *answerModel, "answerMaxTokens": *maxTokens, "answerTokenBudget": *answerBudget}
	if *mode == "answer" {
		b, e := os.ReadFile(*ranks)
		must(e)
		manifest["ranksHash"] = digest(b)
	}
	mb, _ := json.Marshal(manifest)
	hash := digest(mb)
	f, done, err := openOutput(*output, hash)
	must(err)
	defer f.Close()
	if len(done) == 0 {
		must(os.WriteFile(*output+".manifest.json", mb, 0600))
	}
	if *mode == "rank" {
		e := config.LoadEnv()
		q, err := rerank.NewQwen3HTTP(e.RAG.Reranker.Endpoint, e.RAG.Reranker.APIKey, 180*time.Second, 2)
		must(err)
		// A resumed run deducts a deliberately conservative reserve, not just the
		// potentially missing usage of failed calls, from the lifetime budget.
		previous, _ := readRecords(*output)
		reserved := 0.0
		for _, r := range previous {
			for range r.Calls {
				reserved += 32100 * .042 / 1e6
			}
		}
		j, err := rerank.NewJevHTTP("https://openrouter.ai/api/alpha/decisions", os.Getenv("OPENROUTER_API_KEY"), *model, 60*time.Second, *slots, *batch, *budget-reserved)
		must(err)
		rng := rand.New(rand.NewSource(20260921))
		for round := -1; round < *repeats; round++ {
			selected := cases
			phase := "test"
			if round == -1 {
				phase = "warmup"
				selected = cases[:min(*warmups, len(cases))]
			}
			for _, s := range selected {
				order := append([]string{}, arms...)
				rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
				for _, a := range order {
					r := record{Kind: "rank", Manifest: hash, CaseID: s.ID, Arm: a, Repeat: round, Phase: phase, Status: "ok"}
					if done[key(r)] {
						continue
					}
					r.StartedAt = time.Now().UTC()
					texts := []string{}
					for _, c := range s.Candidates {
						texts = append(texts, c.SearchText)
					}
					start := time.Now()
					var e error
					switch a {
					case "qwen3":
						r.Scores, e = q.Rerank(ctx, s.Query, texts, len(texts))
					case "jev":
						r.Scores, r.Calls, e = j.RankDetailed(ctx, s.Query, texts, len(texts))
					case "rrf":
						for i, c := range s.Candidates {
							r.Scores = append(r.Scores, rerank.Result{Index: i, Score: c.Hit.Score})
						}
					}
					r.DurationMS = time.Since(start).Milliseconds()
					if e != nil {
						r.Status = "error"
						r.Error = e.Error()
					}
					emit(f, r)
					done[key(r)] = true
				}
			}
		}
	} else if *mode == "answer" {
		st, _ := db()
		defer st.Close()
		parts := strings.SplitN(*answerModel, "/", 2)
		if len(parts) != 2 {
			panic("answer model requires provider/model")
		}
		p, e := st.GetConfigByName(ctx, "provider", "", "", parts[0])
		must(e)
		if p == nil || !p.Enabled {
			panic("answer provider unavailable")
		}
		var pc config.ProviderConfig
		providerJSON, e := json.Marshal(p.Data)
		must(e)
		must(json.Unmarshal(providerJSON, &pc))
		llm := provider.NewOpenAI(pc.APIKey, pc.APIBase)
		rs, e := readRecords(*ranks)
		must(e)
		byID := map[string]sample{}
		for _, s := range cases {
			byID[s.ID] = s
		}
		tokens := 0
		prev, _ := readRecords(*output)
		for _, r := range prev {
			if r.Answer != nil {
				tokens += r.Answer.Usage.InputTokens + r.Answer.Usage.OutputTokens
			}
		}
		for _, rr := range rs {
			if rr.Phase != "test" || rr.Repeat != 0 || rr.Status != "ok" {
				continue
			}
			s, ok := byID[rr.CaseID]
			if !ok {
				continue
			}
			r := record{Kind: "answer", Manifest: hash, CaseID: s.ID, Arm: rr.Arm, Repeat: 0, Phase: "test", Status: "ok"}
			if done[key(r)] {
				continue
			}
			if tokens >= *answerBudget {
				panic("answer token budget exhausted")
			}
			hits := []rag.Hit{}
			for _, rank := range rr.Scores[:min(5, len(rr.Scores))] {
				if rank.Index < 0 || rank.Index >= len(s.Candidates) {
					panic("invalid rank index")
				}
				c := s.Candidates[rank.Index]
				hits = append(hits, c.Hit)
				r.ContextIDs = append(r.ContextIDs, c.ID)
			}
			r.StartedAt = time.Now().UTC()
			actx, acancel := context.WithTimeout(provider.WithSession(ctx, "jev-bench", *owner, hash, s.ID, rr.Arm), 180*time.Second)
			a, e := rag.GenerateAnswer(actx, llm, rag.AnswerRequest{Mode: rag.AnswerModeEvaluation, Input: rag.AnswerInput{KnowledgeBase: rag.AnswerKnowledgeBase{ID: "eval", Name: "evaluation"}, Question: s.Query, History: s.History, Hits: hits}}, rag.AnswerOptions{Model: parts[1], Temperature: 0, MaxTokens: *maxTokens, PromptBundleVersion: rag.RAGAnswerPromptBundleV1})
			acancel()
			r.DurationMS = a.LatencyMS
			r.Answer = &a
			tokens += a.Usage.InputTokens + a.Usage.OutputTokens
			if e != nil {
				r.Status = "error"
				r.Error = rag.AnswerErrorCode(e)
			}
			emit(f, r)
			done[key(r)] = true
		}
	} else {
		panic("unknown mode")
	}
}
