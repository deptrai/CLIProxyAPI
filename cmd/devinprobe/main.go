package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

const benchSystemPrompt = `You are an impartial and strict benchmark grader.
Given a question, a reference answer (ground truth), and a candidate predicted answer, determine if the candidate answer is correct.

Grading rules:
1. "correct": The predicted answer contains the correct factual answer matching the reference answer. Extra commentary is fine as long as the core fact is correct.
2. "not_attempted": The predicted answer explicitly states that it does not know, cannot find the answer, or refuses to guess.
3. "incorrect": The predicted answer gives a wrong fact, contradicts the reference answer, or fails to provide the required fact.

Respond ONLY with a JSON object:
{
  "verdict": "correct" | "incorrect" | "not_attempted",
  "rationale": "one sentence explanation"
}`

func probe(token, modelUID, systemPrompt, userPrompt string, temp *float64, maxTokens int) {
	prompts := []helps.DevinPrompt{
		{Source: 1, Content: userPrompt},
	}
	protoBytes := helps.BuildDevinGetChatMessageRequest(
		token,
		"",
		modelUID,
		systemPrompt,
		prompts,
		nil,
		temp,
		maxTokens,
		"11111111-2222-3333-4444-555555555555",
		"11111111-2222-3333-4444-555555555555",
		nil,
	)
	framed := helps.WrapConnectEnvelope(protoBytes)
	url := helps.DevinDefaultBaseURL + helps.DevinChatPath

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(framed))
	if err != nil {
		fmt.Println("req build err:", err)
		return
	}
	req.Header.Set("Authorization", "Basic "+token+"-"+token)
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Sentry-Trace", helps.GenerateDevinSentryTrace())
	req.Header["User-Agent"] = []string{""}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("http err:", err)
		return
	}
	defer resp.Body.Close()

	frames := 0
	var outText string
	var stopReason uint64
	var usage *helps.DevinUsage
	for {
		flag, payload, errRead := helps.ReadConnectFrame(resp.Body)
		if errRead != nil {
			if errRead == io.EOF {
				break
			}
			fmt.Println("read err:", errRead)
			break
		}
		frames++
		if flag&helps.ConnectFlagEndStream != 0 {
			code, errTrailer := helps.ParseDevinTrailerError(payload)
			if errTrailer != nil {
				fmt.Printf(">>> TRAILER ERROR http=%d: %v\n", code, errTrailer)
				return
			}
			break
		}
		fr, errParse := helps.ParseDevinFrame(payload)
		if errParse == nil {
			outText += fr.ContentText
			stopReason = fr.StopReason
			if fr.Usage != nil {
				usage = fr.Usage
			}
		}
	}
	fmt.Printf(">>> OK http=%d frames=%d stop=%d in=%d out=%d text=%q\n",
		resp.StatusCode, frames, stopReason,
		func() int64 {
			if usage != nil {
				return usage.PromptTokens
			}
			return -1
		}(),
		func() int64 {
			if usage != nil {
				return usage.CompletionTokens
			}
			return -1
		}(),
		outText,
	)
	_ = start
}

func main() {
	token := os.Getenv("DEVIN_TOKEN")
	if token == "" {
		fmt.Println("DEVIN_TOKEN required")
		os.Exit(1)
	}
	model := os.Getenv("MODEL_UID")
	if model == "" {
		model = "glm-5-3-flash-high"
	}
	tempStr := os.Getenv("TEMP")
	var temp *float64
	if tempStr != "" {
		if f, err := strconv.ParseFloat(tempStr, 64); err == nil {
			temp = &f
		}
	}
	maxTok := 64000
	if v := os.Getenv("MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxTok = n
		}
	}
	sys := benchSystemPrompt
	if os.Getenv("NO_SYSTEM") == "1" {
		sys = ""
	}
	userPrompt := "Question: What was the original scientific name given to *Calytrix acutifolia* by John Lindley in 1839?\nReference Answer: Lhotskya acutifolia\nPredicted Answer: The original scientific name given to *Calytrix acutifolia* by John Lindley in 1839 was Lhotskya acutifolia."
	if v := os.Getenv("USER_PROMPT"); v != "" {
		userPrompt = v
	}
	fmt.Printf("--- probe model=%s temp=%v maxtok=%d sys=%v ---\n", model, tempStr, maxTok, sys != "")
	probe(token, model, sys, userPrompt, temp, maxTok)
}
