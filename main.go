package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ledongthuc/pdf"
)

const cacheFileName = ".autodev_cache.json"
const classificationPrompt = `Ești un router de intenții. Analizează cerința și răspunde DOAR cu 'CODE' sau 'CHAT'.

REGULI:
- Răspunde 'CODE' dacă utilizatorul cere: să ruleze (run), să testeze, să creeze, să modifice, să șteargă, să compileze sau să execute orice comandă.
- Răspunde 'CODE' chiar dacă cerința e vagă dar implică o acțiune tehnică (ex: "e gata?", "verifică dacă merge", "pornește-l").
- Răspunde 'CHAT' DOAR dacă este teorie pură, saluturi sau întrebări despre viață.

EXEMPLE:
- "ce e un pointer?" -> CHAT
- "rulează aplicația" -> CODE
- "testea-l te rog" -> CODE
- "cum funcționează HTTP?" -> CHAT
- "verifică serverul" -> CODE
- "pune-l la treabă" -> CODE
`

type FileCache map[string]string

// --- Funcții de Caching și Citire ---

func computeHash(content []byte) string {
	hash := md5.Sum(content)
	return hex.EncodeToString(hash[:])
}

func loadCache(dir string) FileCache {
	cachePath := filepath.Join(dir, cacheFileName)
	cache := make(FileCache)
	data, err := os.ReadFile(cachePath)
	if err == nil {
		json.Unmarshal(data, &cache)
	}
	return cache
}

func saveCache(dir string, cache FileCache) {
	cachePath := filepath.Join(dir, cacheFileName)
	data, _ := json.MarshalIndent(cache, "", "  ")
	os.WriteFile(cachePath, data, 0644)
}

func getProjectFiles(dir string) string {
	var tree strings.Builder
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && (strings.HasPrefix(info.Name(), ".") || info.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !info.IsDir() {
			relPath, _ := filepath.Rel(dir, path)
			if relPath != cacheFileName && !strings.HasPrefix(info.Name(), "autodev") {
				tree.WriteString("- " + relPath + "\n")
			}
		}
		return nil
	})
	if tree.Len() == 0 {
		return "Niciun fișier găsit.\n"
	}
	return tree.String()
}

func readPDF(path string) (string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf bytes.Buffer
	b, err := r.GetPlainText()
	if err != nil {
		return "", err
	}
	buf.ReadFrom(b)
	return buf.String(), nil
}

func readDocx(path string) (string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer r.Close()
	var textBuilder strings.Builder
	for _, f := range r.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			defer rc.Close()
			decoder := xml.NewDecoder(rc)
			for {
				token, _ := decoder.Token()
				if token == nil {
					break
				}
				switch se := token.(type) {
				case xml.StartElement:
					if se.Name.Local == "t" {
						var text string
						decoder.DecodeElement(&text, &se)
						textBuilder.WriteString(text)
					}
				case xml.EndElement:
					if se.Name.Local == "p" {
						textBuilder.WriteString("\n")
					}
				}
			}
			break
		}
	}
	return textBuilder.String(), nil
}

func getContent(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".txt", ".go", ".py", ".md", ".json", ".js", ".ts":
		content, err := os.ReadFile(path)
		return string(content), err
	case ".pdf":
		return readPDF(path)
	case ".docx":
		return readDocx(path)
	default:
		return "", fmt.Errorf("format nesuportat: %s", ext)
	}
}

// --- SIGURANȚĂ ȘI VALIDARE ---

var forbiddenCommands = []string{
	"rm ", "del ", "rmdir ", "format ", "mkfs", "sudo", "mv ",
}

func isCommandForbidden(cmd string) bool {
	cmdLower := strings.ToLower(strings.TrimSpace(cmd))
	for _, forbidden := range forbiddenCommands {
		if strings.HasPrefix(cmdLower, forbidden) || strings.Contains(cmdLower, " "+forbidden) {
			return true
		}
	}
	return false
}

func askUserConfirmation(actionDesc string) (bool, string) {
	fmt.Printf("\n⚠️  [ATENȚIE] AI-ul Planificator a cerut acțiunea:\n   => %s\n", actionDesc)
	fmt.Print("Aprobare execuție? [y/n] (sau feedback text): ")

	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		answer := strings.TrimSpace(scanner.Text())
		lowerAns := strings.ToLower(answer)
		if lowerAns == "y" || lowerAns == "yes" {
			return true, ""
		} else if lowerAns == "n" || lowerAns == "no" || lowerAns == "" {
			return false, "Utilizatorul a respins execuția."
		} else {
			return false, fmt.Sprintf("Utilizatorul a OPRIT execuția. Feedback: %s", answer)
		}
	}
	return false, "Eroare la citirea deciziei."
}

// Extrage blocul de cod dintre backticks
func extractCodeFromMarkdown(text string) string {
	start := strings.Index(text, "```")
	if start == -1 {
		return strings.TrimSpace(text)
	}
	newline := strings.Index(text[start:], "\n")
	if newline == -1 {
		return strings.TrimSpace(text)
	}
	start = start + newline + 1

	end := strings.LastIndex(text, "```")
	if end <= start {
		return strings.TrimSpace(text[start:])
	}
	return strings.TrimSpace(text[start:end])
}

// --- DEFINIREA TOOL-URILOR DELEGATE (Planificatorul nu scrie cod, doar deleagă) ---

var autoDevTools = []Tool{
	{
		Type: "function",
		Function: ToolFunction{
			Name:        "toolOpenFile",
			Description: "Citește conținutul unui fișier din proiect.",
			Parameters: ToolParameters{
				Type: "object",
				Properties: map[string]ToolProperty{
					"filePath": {
						Type:        "string",
						Description: "Calea către fișierul care trebuie citit,foloseste calea relativa",
					},
				},
				Required: []string{"filePath"},
			},
		},
	},
	{
		Type: "function",
		Function: ToolFunction{
			Name:        "toolExecuteCommand",
			Description: "Execută o comandă in terminal. ",
			Parameters: ToolParameters{
				Type: "object",
				Properties: map[string]ToolProperty{
					"command": {Type: "string", Description: "Comanda de executat in terminal"},
				},
				Required: []string{"command"},
			},
		},
	},
	{
		Type: "function",
		Function: ToolFunction{
			Name:        "toolCreateFile",
			Description: "Deleagă crearea unui fișier către AI-ul Coder Expert. Tu doar îi dai instrucțiuni despre ce să scrie.",
			Parameters: ToolParameters{
				Type: "object",
				Properties: map[string]ToolProperty{
					"filePath":     {Type: "string", Description: "Calea către fișierul ce trebuie creat."},
					"instructions": {Type: "string", Description: "Instrucțiuni detaliate pentru AI-ul Coder despre ce cod trebuie să scrie în acest fișier."},
				},
				Required: []string{"filePath", "instructions"},
			},
		},
	},
	{
		Type: "function",
		Function: ToolFunction{
			Name:        "toolEditFile",
			Description: "Deleagă rescrierea unui fișier existent către AI-ul Coder Expert pentru a repara o eroare sau a adăuga ceva.",
			Parameters: ToolParameters{
				Type: "object",
				Properties: map[string]ToolProperty{
					"filePath":     {Type: "string", Description: "Calea către fișierul de modificat."},
					"instructions": {Type: "string", Description: "Explică-i AI-ului Coder ce eroare a apărut sau ce modificare logică trebuie aplicată întregului cod din fișier."},
				},
				Required: []string{"filePath", "instructions"},
			},
		},
	},
}

// --- IMPLEMENTARE TOOL-URI (LOGICA MASTER-WORKER) ---

func handleToolCall(toolName string, args map[string]interface{}, coderModel string, projectContext string) string {
	switch toolName {

	case "toolExecuteCommand":
		cmdRaw, ok := args["command"].(string)
		if !ok {
			return "Eroare: Argumentul 'command' lipsește."
		}
		if isCommandForbidden(cmdRaw) {
			return "Eroare: Comanda este interzisă."
		}

		approved, feedback := askUserConfirmation(fmt.Sprintf("Execută comandă: `%s`", cmdRaw))
		if !approved {
			return feedback
		}

		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/c", cmdRaw)
		} else {
			cmd = exec.Command("bash", "-c", cmdRaw)
		}

		output, err := cmd.CombinedOutput()
		result := string(output)
		if err != nil {
			result += fmt.Sprintf("\nEroare: %v", err)
		}
		return result

	case "toolCreateFile":
		path, _ := args["filePath"].(string)
		instructions, _ := args["instructions"].(string)

		approved, feedback := askUserConfirmation(fmt.Sprintf("Creare fișier: `%s`\n   Instrucțiuni trimise către Qwen: %s", path, instructions))
		if !approved {
			return feedback
		}

		if _, err := os.Stat(path); err == nil {
			return fmt.Sprintf("Eroare: Fișierul %s există deja. Folosește toolEditFile.", path)
		}

		fmt.Printf("\n[Sub-Agent] 🧠 Apelez %s pentru a genera codul...\n", coderModel)
		prompt := fmt.Sprintf(`
Ești un expert programator.

Ai acces la următorul proiect existent.

================= PROIECT EXISTENT =================
%s
====================================================

Scrie codul complet pentru fișierul cerut.

CERINȚĂ:
%s

Returnează DOAR codul într-un bloc markdown.
`, projectContext, instructions)

		resp, err := CallOllamaGenerate(coderModel, prompt)
		if err != nil {
			return fmt.Sprintf("Eroare la generarea codului cu %s: %v", coderModel, err)
		}

		cleanCode := extractCodeFromMarkdown(resp)

		dir := filepath.Dir(path)
		os.MkdirAll(dir, 0755)
		if err := os.WriteFile(path, []byte(cleanCode), 0644); err != nil {
			return fmt.Sprintf("Eroare la scriere pe disc: %v", err)
		}
		return fmt.Sprintf("Fișierul a fost generat de %s și salvat cu succes.", coderModel)

	case "toolEditFile":
		path, _ := args["filePath"].(string)
		instructions, _ := args["instructions"].(string)

		approved, feedback := askUserConfirmation(fmt.Sprintf("Modificare fișier: `%s`\n   Instrucțiuni reparare către Qwen: %s", path, instructions))
		if !approved {
			return feedback
		}

		currentContent, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("Eroare citire: %v", err)
		}

		fmt.Printf("\n[Sub-Agent] 🧠 Apelez %s pentru a repara/modifica fișierul...\n", coderModel)
		prompt := fmt.Sprintf(`
Ești un expert programator.

Acesta este proiectul complet:

================= PROIECT =================
%s
===========================================

Trebuie să modifici următorul fișier.

CERINȚĂ MODIFICARE:
%s

FIȘIER CURENT: %s

Returnează DOAR fișierul complet modificat într-un bloc markdown.
`, projectContext, instructions, string(currentContent))

		resp, err := CallOllamaGenerate(coderModel, prompt)
		if err != nil {
			return fmt.Sprintf("Eroare la modificarea codului cu %s: %v", coderModel, err)
		}

		cleanCode := extractCodeFromMarkdown(resp)

		if err := os.WriteFile(path, []byte(cleanCode), 0644); err != nil {
			return fmt.Sprintf("Eroare la salvarea fișierului modificat: %v", err)
		}
		return fmt.Sprintf("Fișierul a fost rescris cu succes de %s conform instrucțiunilor.", coderModel)
	case "toolOpenFile":
		path, ok := args["filePath"].(string)
		if !ok {
			return "Eroare: calea lipsește."
		}

		// Curățăm calea: dacă primim o cale absolută, luăm doar numele fișierului
		// sau îl forțăm să fie relativ la folderul curent
		cleanPath := filepath.Base(path)

		content, err := os.ReadFile(cleanPath)
		if err != nil {
			return fmt.Sprintf("Eroare: Fișierul '%s' nu a fost găsit în folderul curent. Fișiere disponibile: \n%s", cleanPath, getProjectFiles("."))
		}
		return string(content)
	default:
		return fmt.Sprintf("Eroare: Tool-ul %s nu este recunoscut.", toolName)
	}
}

type InlineToolCall struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
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
func classifyIntent(userInput string, modelName string) string {
	userInputLower := strings.ToLower(userInput)

	// Plasa de siguranță: Cuvinte cheie care forțează modul CODE
	actionKeywords := []string{"ruleaza", "rulează", "test", "executa", "porneste", "pornește", "run", "go run", "testeaza"}
	for _, kw := range actionKeywords {
		if strings.Contains(userInputLower, kw) {
			return "CODE"
		}
	}
	reqBody := ChatRequest{
		Model: modelName,
		Messages: []ChatMessage{
			{
				Role:    "system",
				Content: "Ești un asistent care clasifică intenția utilizatorului. Răspunde DOAR cu cuvântul 'CHAT' dacă utilizatorul pune întrebări teoretice, de cultură generală sau vrea o discuție simplă. Răspunde DOAR cu 'CODE' dacă utilizatorul cere crearea de fișiere, modificarea codului, rularea proiectului sau repararea unor erori.",
			},
			{Role: "user", Content: userInput},
		},
		Stream: false,
	}

	jsonData, _ := json.Marshal(reqBody)
	resp, err := http.Post(ollamaBaseURL+"/chat", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return "CHAT" // Fallback la chat în caz de eroare
	}
	defer resp.Body.Close()

	var chatResp ChatResponse
	json.NewDecoder(resp.Body).Decode(&chatResp)

	intent := strings.TrimSpace(strings.ToUpper(chatResp.Message.Content))
	if strings.Contains(intent, "CODE") {
		return "CODE"
	}
	return "CHAT"
}
func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("Eroare: %v\n", err)
		return
	}

	// --- 1. SCANARE ---
	cache := loadCache(cwd)
	newCache := make(FileCache)
	var contextBuilder strings.Builder

	filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			relPath, _ := filepath.Rel(cwd, path)
			if relPath != cacheFileName && !strings.HasPrefix(info.Name(), "autodev") && !strings.Contains(path, "node_modules") {
				textContent, _ := getContent(path)

				currentHash := computeHash([]byte(textContent))
				newCache[relPath] = currentHash

				// Folosim variabila cache pentru a verifica dacă fișierul este nou/modificat
				oldHash, exists := cache[relPath]
				if !exists || oldHash != currentHash {
					fmt.Printf("[Indexat] %s\n", relPath)
				}

				contextBuilder.WriteString(fmt.Sprintf("\n--- Fișier: %s ---\n%s\n", relPath, textContent))
			}
		}
		return nil
	})
	saveCache(cwd, newCache)

	// --- 2. DETECTARE MODELE PENTRU ARHITECTURA MASTER-WORKER ---
	modele, err := GetLocalModels()
	if err != nil || len(modele) == 0 {
		fmt.Println("⚠️ Eroare Ollama sau niciun model instalat.")
		return
	}

	var plannerModel, coderModel string
	for _, m := range modele {
		if strings.Contains(strings.ToLower(m), "llama") && plannerModel == "" {
			plannerModel = m
		}
		if strings.Contains(strings.ToLower(m), "qwen") && coderModel == "" {
			coderModel = m
		}
	}
	// Fallback dacă nu găsește llama/qwen explicit
	if plannerModel == "" {
		plannerModel = modele[0]
	}
	if coderModel == "" {
		coderModel = modele[0]
	}

	fmt.Println("\n====================================================")
	fmt.Println("🌟 AUTODEV: ARHITECTURĂ MASTER-WORKER ACTIVATĂ 🌟")
	fmt.Printf("🧠 Planificator (Interfață): %s\n", plannerModel)
	fmt.Printf("💻 Executant (Scriere Cod): %s\n", coderModel)
	fmt.Println("====================================================")

	scanner := bufio.NewScanner(os.Stdin)
	var conversationHistory []ChatMessage

	for {
		fmt.Print("\n> Cerința ta: ")
		if !scanner.Scan() {
			break
		}
		userInput := scanner.Text()
		if strings.ToLower(userInput) == "exit" {
			break
		}
		if strings.TrimSpace(userInput) == "" {
			continue
		}

		// 1. Clasificăm intenția
		intent := classifyIntent(userInput, plannerModel)
		fmt.Printf("[Sistem] Intenție detectată: %s\n", intent)

		if intent == "CHAT" {
			// --- LOGICA DE CHAT SIMPLU ---
			mesajeChat := append(conversationHistory, ChatMessage{Role: "user", Content: userInput})

			// Prompt simplu, fără tool-uri
			mesajeChat = append([]ChatMessage{{
				Role:    "system",
				Content: "Ești un asistent util. Răspunde direct la întrebarea utilizatorului. Nu menționa fișierele din proiect dacă nu ești întrebat de ele.",
			}}, mesajeChat...)

			responseMsg, err := CallOllamaChat(plannerModel, mesajeChat, nil) // nil = fără tool-uri
			if err == nil {
				fmt.Println("\n🗣️ [Asistent]:", responseMsg.Content)
				conversationHistory = append(conversationHistory, ChatMessage{Role: "user", Content: userInput}, responseMsg)
			}
			continue // Trecem la următoarea întrebare, sărim peste bucla de iterații/tool-uri
		}

		// --- LOGICA DE CODING (Dacă intent == "CODE") ---
		conversationHistory = append(conversationHistory, ChatMessage{Role: "user", Content: userInput})

		maxIterations := 5
		for currentIteration := 1; currentIteration <= maxIterations; currentIteration++ {
			var promptBuilder strings.Builder
			promptBuilder.WriteString("Ești AutoDev, un manager de proiect software inteligent.\n")
			promptBuilder.WriteString("Tu NU scrii cod direct. Rolul tău este să interacționezi cu utilizatorul și să de-legi munca asistentului tău expert în cod (Coder).\n")
			promptBuilder.WriteString("### RESTRICȚII STRICTE PENTRU PLANIFICATOR:\n")
			promptBuilder.WriteString("1. DISPONIBILITATE TOOL-URI: Ai voie să folosești DOAR: toolOpenFile, toolCreateFile, toolEditFile și toolExecuteCommand. NU inventa tool-uri precum 'toolListFiles' sau 'toolSearch'.\n")
			promptBuilder.WriteString("2. CĂI DE FIȘIERE: Folosește DOAR căi relative (ex: 'main.go', 'data/config.json'). NU folosi căi absolute de tipul '/home/user/...' sau 'C:\\Users\\...'.\n")
			promptBuilder.WriteString("3. MEMORIE: Lista de fișiere de mai jos este REALA și ACTUALĂ. Dacă un fișier nu apare în listă, înseamnă că NU EXISTĂ.\n")
			promptBuilder.WriteString("4. LOGICĂ: Dacă ai citit deja un fișier, informația se află în istoricul conversației. Nu îl mai citi a doua oară inutil.\n")
			promptBuilder.WriteString("5. LIMBAJ: Dacă utilizatorul întreabă ceva despre Python dar tu vezi în listă doar fișiere Go, explică-i că proiectul este în Go.\n")
			promptBuilder.WriteString("\n[STAREA ACTUALĂ A PROIECTULUI]\n")
			promptBuilder.WriteString("Fișiere prezente pe disc:\n")
			promptBuilder.WriteString("--------------------------\n")
			promptBuilder.WriteString(getProjectFiles(cwd)) // Funcția ta care listează fișierele
			promptBuilder.WriteString("--------------------------\n")
			promptBuilder.WriteString("\nIMPORTANT: aceste fișiere există deja. Nu le recrea.\n")

			promptBuilder.WriteString("\nCONȚINUT FIȘIERE INDEXATE:\n")
			promptBuilder.WriteString(contextBuilder.String())

			mesajePentruOllama := make([]ChatMessage, len(conversationHistory))
			copy(mesajePentruOllama, conversationHistory)

			lastUserIdx := -1
			for i := len(mesajePentruOllama) - 1; i >= 0; i-- {
				if mesajePentruOllama[i].Role == "user" {
					lastUserIdx = i
					break
				}
			}
			if lastUserIdx != -1 {
				originalContent := mesajePentruOllama[lastUserIdx].Content
				mesajePentruOllama[lastUserIdx].Content = promptBuilder.String() + "CERINȚA CURENTĂ/REZULTAT:\n" + originalContent
			}

			fmt.Printf("\n[Sistem] Aștept decizia de la %s...\n", plannerModel)
			responseMsg, err := CallOllamaChat(plannerModel, mesajePentruOllama, autoDevTools)
			if err != nil {
				fmt.Printf("\n[Eroare]: %v\n", err)
				conversationHistory = conversationHistory[:len(conversationHistory)-1]
				break
			}

			conversationHistory = append(conversationHistory, responseMsg)
			toolExecuted := false

			// 1. Verificare ToolCalls native
			if len(responseMsg.ToolCalls) > 0 {
				for _, tc := range responseMsg.ToolCalls {
					toolResult := handleToolCall(tc.Function.Name, tc.Function.Arguments, coderModel, contextBuilder.String())
					fmt.Printf("[Rezultat Tool]:\n%s\n", toolResult)
					conversationHistory = append(conversationHistory, ChatMessage{Role: "tool", Content: toolResult})
					toolExecuted = true
				}
			} else {
				// 2. Fallback JSON
				jsonStr, found := extractJSONFromText(responseMsg.Content)
				if found {
					var inlineTool InlineToolCall
					if err := json.Unmarshal([]byte(jsonStr), &inlineTool); err == nil && inlineTool.Name != "" {
						toolResult := handleToolCall(inlineTool.Name, inlineTool.Arguments, coderModel, contextBuilder.String())
						fmt.Printf("[Rezultat Tool]:\n%s\n", toolResult)
						conversationHistory = append(conversationHistory, ChatMessage{
							Role:    "user",
							Content: fmt.Sprintf("Am executat %s. Rezultat:\n%s\nAnalizează și continuă/confirmă.", inlineTool.Name, toolResult),
						})
						toolExecuted = true
					}
				}
			}

			if !toolExecuted {
				fmt.Println("\n🗣️ [Răspuns Llama / Interfață]:")
				fmt.Println("----------------------------------------------------")
				fmt.Println(responseMsg.Content)
				fmt.Println("----------------------------------------------------")
				break
			}
		}
	}
}
