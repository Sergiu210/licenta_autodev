package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// --- Structuri ---

type ModelListResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

type ChatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type ToolCall struct {
	Function struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	} `json:"function"`
}

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	//Tools    []Tool                 `json:"tools,omitempty"`
	Options map[string]interface{} `json:"options,omitempty"` // <-- Pentru context 32k
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  ToolParameters `json:"parameters"`
}

type ToolParameters struct {
	Type       string                  `json:"type"`
	Properties map[string]ToolProperty `json:"properties"`
	Required   []string                `json:"required"`
}

type ToolProperty struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type ChatResponse struct {
	Message ChatMessage `json:"message"`
}

// Structuri pentru generarea brută de cod (Qwen)
type GenerateRequest struct {
	Model   string                 `json:"model"`
	Prompt  string                 `json:"prompt"`
	Stream  bool                   `json:"stream"`
	Options map[string]interface{} `json:"options,omitempty"`
}

type GenerateResponse struct {
	Response string `json:"response"`
}

// --- Funcții ---

func GetLocalModels(baseURL string) ([]string, error) {
	resp, err := http.Get(baseURL + "/tags")
	if err != nil {
		return nil, fmt.Errorf("eroare la conectarea cu Ollama: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	fmt.Println("========== OLLAMA RAW RESPONSE ==========")
	fmt.Println(string(body))
	fmt.Println("=========================================")
	var listResp ModelListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		return nil, err
	}

	var models []string
	for _, m := range listResp.Models {
		models = append(models, m.Name)
	}
	return models, nil
}

// CallOllamaChat este folosit de Llama3.1 (Planificatorul)
func CallOllamaChat(baseURL string, modelName string, messages []ChatMessage, tools []Tool) (ChatMessage, error) {
	reqBody := ChatRequest{
		Model:    modelName,
		Messages: messages,
		Stream:   false,
		//Tools:    tools,
		Options: map[string]interface{}{
			"num_ctx": 16384, // Context generos pentru chat (16k)
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return ChatMessage{}, err
	}

	resp, err := http.Post(baseURL+"/chat", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return ChatMessage{}, fmt.Errorf("eroare HTTP: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatMessage{}, err
	}
	fmt.Println("\n========== OLLAMA RAW GENERATE ==========")
	fmt.Println(string(body))
	fmt.Println("=========================================")
	var chatResp ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return ChatMessage{}, fmt.Errorf("eroare parsare JSON răspuns: %v", err)
	}

	return chatResp.Message, nil
}

// CallOllamaGenerate este folosit de Qwen2.5-Coder (Executantul)
func CallOllamaGenerate(baseURL string, modelName string, prompt string) (string, error) {
	// ... structura reqBody rămâne la fel ...
	reqBody := GenerateRequest{
		Model:  modelName,
		Prompt: prompt,
		Stream: false,
		Options: map[string]interface{}{
			"num_ctx": 32768, // Context maxim pentru Coder (32k)
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	resp, err := http.Post(baseURL+"/generate", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("eroare la apelarea modelului: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var genResp GenerateResponse
	if err := json.Unmarshal(body, &genResp); err != nil {
		return "", fmt.Errorf("eroare la parsarea răspunsului Ollama: %v", err)
	}

	return genResp.Response, nil
}

// --- LLM ROUTER (Gateway către diverse backend-uri) ---

func CallChat(cfg *Config, messages []ChatMessage, tools []Tool) (ChatMessage, error) {
	switch cfg.Provider {
	case "ollama":
		// Folosește funcția ta existentă
		return CallOllamaChat(cfg.BaseURL, cfg.PlannerModel, messages, tools)

	case "openai":
		// Aici vei implementa CallOpenAIChat în viitor
		return ChatMessage{}, fmt.Errorf("OpenAI nu este încă implementat")

	case "gemini":
		// Aici vei implementa CallGeminiChat în viitor
		return ChatMessage{}, fmt.Errorf("Gemini nu este încă implementat")

	case "vllm":
		// vLLM folosește de obicei formatul compatibil cu OpenAI
		return ChatMessage{}, fmt.Errorf("vLLM nu este încă implementat")

	default:
		return ChatMessage{}, fmt.Errorf("provider necunoscut: %s", cfg.Provider)
	}
}

func CallGenerate(cfg *Config, prompt string) (string, error) {
	switch cfg.Provider {
	case "ollama":
		// Folosește funcția ta existentă
		return CallOllamaGenerate(cfg.BaseURL, cfg.CoderModel, prompt)

	case "openai":
		// Aici vei implementa CallOpenAIGenerate
		return "", fmt.Errorf("OpenAI nu este încă implementat")

	// Adaugi restul pe parcurs...
	default:
		return "", fmt.Errorf("provider necunoscut: %s", cfg.Provider)
	}
}
func extractJSONFromText(text string) (string, bool) {
	blockStart := strings.Index(text, "```json")
	if blockStart != -1 {
		jsonStart := strings.Index(text[blockStart:], "{")
		if jsonStart != -1 {
			absStart := blockStart + jsonStart
			blockEnd := strings.Index(text[absStart:], "```")
			if blockEnd != -1 {
				return strings.TrimSpace(text[absStart : absStart+blockEnd]), true
			}
		}
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start != -1 && end != -1 && end > start {
		return strings.TrimSpace(text[start : end+1]), true
	}
	return "", false
}
