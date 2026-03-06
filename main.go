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
	"io"
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

type PlanStep struct {
	Action       string `json:"action"`       // "create_file", "edit_file", "execute_command"
	FilePath     string `json:"file_path"`    // Calea fișierului (dacă e cazul)
	Instructions string `json:"instructions"` // Ce trebuie să facă Qwen sau ce comandă să ruleze
}

type ExecutionPlan struct {
	Steps []PlanStep `json:"steps"`
}

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

	case "toolCreateDirectory":
		path, _ := args["dirPath"].(string)

		if err := os.MkdirAll(path, 0755); err != nil {
			return fmt.Sprintf("Eroare la crearea directorului %s: %v", path, err)
		}
		return fmt.Sprintf("Directorul `%s` a fost creat cu succes.", path)

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

		approved, feedback := askUserConfirmation(fmt.Sprintf("Creare fișier(e) pornind de la: `%s`\n   Instrucțiuni trimise către Qwen: %s", path, instructions))
		if !approved {
			return feedback
		}

		fmt.Printf("\n[Sub-Agent] 🧠 Apelez %s pentru a genera codul...\n", coderModel)

		// 1. Modificăm promptul pentru a impune un format strict multi-fișier
		prompt := fmt.Sprintf(`Ești un expert programator.
Ai acces la următorul proiect existent.

================= PROIECT EXISTENT =================
%s
====================================================

CERINȚĂ:
%s

Calea principală sugerată pentru creare este: %s

IMPORTANT: Trebuie să returnezi codul folosind EXACT acest format pentru FIECARE fișier pe care îl generezi (chiar dacă generezi unul singur). Nu adăuga alte explicații, returnează DOAR blocurile în acest format:

[FILE: calea/catre/fisier.ext]
%s
codul complet aici
%s
`, projectContext, instructions, path, "```", "```")

		resp, err := CallOllamaGenerate(coderModel, prompt)
		if err != nil {
			return fmt.Sprintf("Eroare la generarea codului cu %s: %v", coderModel, err)
		}

		// 2. Logica de extragere a mai multor fișiere
		filesCreated := 0
		var outputLog strings.Builder

		// Împărțim răspunsul după marcajul nostru custom
		parts := strings.Split(resp, "[FILE: ")

		// Dacă modelul nu a respectat deloc formatul (Fallback pentru un singur fișier)
		if len(parts) <= 1 && !strings.Contains(resp, "[FILE:") {
			cleanCode := extractCodeFromMarkdown(resp)
			dir := filepath.Dir(path)
			os.MkdirAll(dir, 0755)
			if err := os.WriteFile(path, []byte(cleanCode), 0644); err != nil {
				return fmt.Sprintf("Eroare la scriere pe disc pentru %s: %v", path, err)
			}
			return fmt.Sprintf("✅ Fișierul `%s` a fost generat (Fallback single-file).", path)
		}

		// 3. Iterăm prin blocurile găsite și le salvăm pe disc
		for _, part := range parts {
			if strings.TrimSpace(part) == "" {
				continue // Sărim peste bucățile goale (ex: dinainte de primul [FILE:)
			}

			// Căutăm unde se închide paranteza pentru calea fișierului
			endBracket := strings.Index(part, "]")
			if endBracket == -1 {
				continue
			}

			// Extragem calea și restul textului (care e codul)
			filePath := strings.TrimSpace(part[:endBracket])
			fileContentRaw := part[endBracket+1:]

			// Folosim funcția ta existentă ca să scoatem codul efectiv dintre backticks (```)
			cleanCode := extractCodeFromMarkdown(fileContentRaw)
			if cleanCode == "" {
				continue
			}

			// Salvăm pe disc
			dir := filepath.Dir(filePath)
			os.MkdirAll(dir, 0755) // Creăm automat folderele dacă nu există

			if err := os.WriteFile(filePath, []byte(cleanCode), 0644); err != nil {
				outputLog.WriteString(fmt.Sprintf("❌ Eroare salvare `%s`: %v\n", filePath, err))
				continue
			}

			outputLog.WriteString(fmt.Sprintf("✅ `%s` creat cu succes.\n", filePath))
			filesCreated++
		}

		if filesCreated == 0 {
			return "⚠️ LLM-ul a răspuns, dar nu s-au putut extrage fișiere valide. Incearcă să reformulezi cerința."
		}

		return fmt.Sprintf("Operațiune încheiată. %d fișiere salvate:\n%s", filesCreated, outputLog.String())
	case "toolEditFile":
		path, _ := args["filePath"].(string)
		instructions, _ := args["instructions"].(string)
		if isFileProtected(path) {
			return "Eroare: Acces interzis. Nu poți modifica fișiere de sistem sau biblioteci externe!"
		}

		approved, feedback := askUserConfirmation(fmt.Sprintf("Modificare fișier: `%s`\n   Instrucțiuni reparare către Coder: %s", path, instructions))
		if !approved {
			return feedback
		}

		currentContent, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("Eroare citire: %v", err)
		}

		fmt.Printf("\n[Sub-Agent] 🧠 Apelez %s pentru a repara/modifica fișierul...\n", coderModel)

		// Prompt OPTIMIZAT: Fără 'projectContext', doar conținutul strict al fișierului!
		prompt := fmt.Sprintf(`Ești un expert programator.

Trebuie să modifici următorul fișier conform cerinței.

================= FIȘIER CURENT (%s) =================
%s
======================================================

CERINȚĂ MODIFICARE:
%s

REGULI:
1. Returnează DOAR codul complet modificat pentru acest fișier într-un bloc markdown (fără explicații suplimentare).
2. Păstrează restul codului din fișier intact, modifică doar ce se cere.
`, path, string(currentContent), instructions)

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
func generateExecutionPlan(plannerModel, userInput, projectTree string) (*ExecutionPlan, error) {
	prompt := fmt.Sprintf(`Ești arhitectul strict al unui sistem AI. 
Scopul tău este să generezi un JSON cu pași, analizând harta de fișiere de mai jos.

ARBORELE FIȘIERELOR EXISTENTE (Nu ai acces la conținutul lor, doar la nume):
===========================
%s
===========================

CERINȚA UTILIZATORULUI: "%s"

REGULI ABSOLUTE DE PLANIFICARE:
1. ALEGE FIȘIERUL: Dacă utilizatorul cere o modificare, uită-te în "ARBORELE FIȘIERELOR EXISTENTE". Dacă fișierul apare acolo, folosește OBLIGATORIU acțiunea "edit_file" cu calea sa exactă.
2. CUM FUNCȚIONEAZĂ "edit_file": Când tu dai comanda "edit_file", sistemul nostru din spate va citi automat conținutul fișierului de pe disc și îl va trimite Coder-ului. Tu trebuie doar să îi scrii instrucțiunile logice (ex: "Modifică culoarea titlului în roșu").
3. CREARE: Folosește "create_file" DOAR dacă fișierul nu există în arbore și a fost cerut explicit.
4. FORMAT: Returnează DOAR un JSON valid, fără explicații.

Exemplu format așteptat:
{
  "steps": [
    {
      "action": "edit_file",
      "file_path": "blog/post.html",
      "instructions": "Schimbă culoarea titlului H1 în roșu folosind CSS inline sau style."
    }
  ]
}
`, projectTree, userInput)
	reqBody := ChatRequest{
		Model: plannerModel,
		Messages: []ChatMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
		Options: map[string]interface{}{
			"temperature": 0.1, // Temperatură mică pentru a forța un output determinist
		},
	}

	// Notă: Ollama suportă forțarea JSON-ului dacă specifici formatul în request,
	// dar la nivelul API-ului curent din `ChatRequest`-ul tău, adăugăm manual în structură dacă e nevoie,
	// sau ne bazăm pe instrucțiunile din prompt.

	jsonData, _ := json.Marshal(reqBody)
	resp, err := http.Post(ollamaBaseURL+"/chat", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var chatResp ChatResponse
	json.Unmarshal(body, &chatResp)

	// Extragem JSON-ul din răspuns în caz că modelul a mai adăugat text "Here is the plan:"
	jsonStr, found := extractJSONFromText(chatResp.Message.Content)
	if !found {
		// Fallback: încercăm să parsăm direct tot răspunsul
		jsonStr = chatResp.Message.Content
	}

	var plan ExecutionPlan
	if err := json.Unmarshal([]byte(jsonStr), &plan); err != nil {
		return nil, fmt.Errorf("Eroare la parsarea planului JSON: %v. Răspuns brut: %s", err, jsonStr)
	}

	return &plan, nil
}
func isFileProtected(path string) bool {
	// Listează căile sau directoarele care NU trebuie atinse niciodată
	protectedPaths := []string{
		"AppData",
		"site-packages",
		"Lib\\site-packages",
		"Python313",
	}
	for _, p := range protectedPaths {
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}
func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("Eroare: %v\n", err)
		return
	}

	// --- DETECTARE MODELE PENTRU ARHITECTURA MASTER-WORKER ---
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
	var autoPrompt string // <--- Variabilă pentru Auto-Reparare

	for {
		// --- 1. SCANARE PROASPĂTĂ (Mutată aici pentru a citi mereu modificările noi) ---
		cache := loadCache(cwd)
		newCache := make(FileCache)
		var contextBuilder strings.Builder
		// --- 1. SCANARE PROASPĂTĂ (Construim doar ARBORELE de fișiere) ---
		cache = loadCache(cwd)
		newCache = make(FileCache)

		var treeBuilder strings.Builder
		treeBuilder.WriteString("📁 Rădăcină Proiect:\n")

		filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}

			// IGNORĂM complet folderele mari/inutile pentru a nu pierde timpul
			if info.IsDir() {
				n := info.Name()
				if n == "node_modules" || n == ".git" || n == ".venv" || n == "venv" || n == "__pycache__" {
					return filepath.SkipDir
				}
				return nil
			}

			// Dacă este fișier valid
			relPath, _ := filepath.Rel(cwd, path)
			if relPath != cacheFileName && !strings.HasPrefix(info.Name(), "autodev") {

				// Păstrăm logica ta de cache pentru a afișa ce s-a modificat pe disc
				textContent, _ := getContent(path)
				currentHash := computeHash([]byte(textContent))
				newCache[relPath] = currentHash

				oldHash, exists := cache[relPath]
				if !exists || oldHash != currentHash {
					fmt.Printf("[Detectat/Modificat] 📄 %s\n", relPath)
				}

				// ADĂUGĂM DOAR CALEA ÎN ARBORE (Fără conținut!)
				treeBuilder.WriteString(fmt.Sprintf("  - %s\n", relPath))
			}
			return nil
		})
		saveCache(cwd, newCache)

		// Salvăm structura pentru a o trimite Planificatorului
		projectTree := treeBuilder.String()

		// --- 2. CITIRE INPUT SAU AUTO-PROMPT ---
		var userInput string
		if autoPrompt != "" {
			userInput = autoPrompt
			fmt.Println("\n🔄 [Auto-Recuperare] Trimit automat eroarea către Planificator...")
			autoPrompt = "" // Resetăm pentru a nu bucla la infinit
		} else {
			fmt.Print("\n> Cerința ta: ")
			if !scanner.Scan() {
				break
			}
			userInput = scanner.Text()
		}

		if strings.ToLower(userInput) == "exit" {
			break
		}
		if strings.TrimSpace(userInput) == "" {
			continue
		}

		// 3. Clasificăm intenția
		intent := classifyIntent(userInput, plannerModel)
		fmt.Printf("[Sistem] Intenție detectată: %s\n", intent)

		if intent == "CHAT" {
			// --- LOGICA DE CHAT SIMPLU ---
			mesajeChat := append(conversationHistory, ChatMessage{Role: "user", Content: userInput})

			mesajeChat = append([]ChatMessage{{
				Role:    "system",
				Content: "Ești un asistent util. Răspunde direct la întrebarea utilizatorului. Nu menționa fișierele din proiect dacă nu ești întrebat de ele.",
			}}, mesajeChat...)

			responseMsg, err := CallOllamaChat(plannerModel, mesajeChat, nil)
			if err == nil {
				fmt.Println("\n🗣️ [Asistent]:", responseMsg.Content)
				conversationHistory = append(conversationHistory, ChatMessage{Role: "user", Content: userInput}, responseMsg)
			}
			continue
		}

		// --- LOGICA DE CODING (Intent == "CODE") ---
		fmt.Printf("\n[Planificator] 🧠 `%s` analizează cerința și creează planul de acțiune...\n", plannerModel)

		plan, err := generateExecutionPlan(plannerModel, userInput, projectTree)
		if err != nil {
			fmt.Printf("❌ Nu am putut genera planul: %v\n", err)
			continue
		}

		if len(plan.Steps) == 0 {
			fmt.Println("⚠️ Planul generat este gol. Probabil cerința a fost neclară.")
			continue
		}

		fmt.Println("\n📋 PLAN DE EXECUȚIE DETECTAT:")
		for i, step := range plan.Steps {
			// Sărim peste pașii care nu au instrucțiuni valide (failsafe pentru halucinațiile Llama)
			if strings.TrimSpace(step.Instructions) == "" {
				continue
			}
			fmt.Printf("  Pas %d: [%s] -> %s\n", i+1, step.Action, step.Instructions)
		}
		fmt.Println("----------------------------------------------------")

		// BACKEND-UL preia controlul. LLAMA NU MAI INTERVINE.
		eroareDetectata := false // Flag pentru a ști cum mapăm istoricul

		for i, step := range plan.Steps {
			if strings.TrimSpace(step.Instructions) == "" {
				continue // Ignorăm și la execuție
			}
			fmt.Printf("\n🚀 Execut Pasul %d: %s\n", i+1, step.Action)

			var toolResult string
			args := make(map[string]interface{})

			switch step.Action {
			case "create_directory": // <--- Adăugat
				args["dirPath"] = step.FilePath
				toolResult = handleToolCall("toolCreateDirectory", args, coderModel, contextBuilder.String())
			case "create_file":
				args["filePath"] = step.FilePath
				args["instructions"] = step.Instructions
				toolResult = handleToolCall("toolCreateFile", args, coderModel, contextBuilder.String())

			case "edit_file":
				args["filePath"] = step.FilePath
				args["instructions"] = step.Instructions
				toolResult = handleToolCall("toolEditFile", args, coderModel, contextBuilder.String())

			case "execute_command":
				args["command"] = step.Instructions
				toolResult = handleToolCall("toolExecuteCommand", args, coderModel, contextBuilder.String())

			default:
				toolResult = fmt.Sprintf("Acțiune necunoscută: %s", step.Action)
			}

			fmt.Printf("[Rezultat Pas %d]:\n%s\n", i+1, toolResult)

			// Verificăm dacă utilizatorul a anulat intenționat
			if strings.Contains(toolResult, "OPRIT") || strings.Contains(toolResult, "respins") {
				fmt.Println("\n⚠️ Execuția planului a fost întreruptă de utilizator.")
				eroareDetectata = true
				break
			}

			// Verificăm dacă a apărut o eroare de execuție/sintaxă
			if strings.Contains(toolResult, "Eroare") || strings.Contains(toolResult, "error") || strings.Contains(toolResult, "Exception") {
				fmt.Printf("\n⚠️ Eroare detectată la Pasul %d! Pregătesc auto-repararea...\n", i+1)

				// Formulăm mesajul automat pentru Llama
				autoPrompt = fmt.Sprintf(`La executarea planului a apărut următoarea eroare în terminal:
%s

Te rog analizează cu atenție eroarea și codul actual, identifică problema și creează un plan NOU cu pași de tip "edit_file" pentru a repara eroarea. 
Apoi, adaugă un pas final de tip "execute_command" pentru a rula din nou și a verifica dacă s-a rezolvat.`, toolResult)

				eroareDetectata = true
				break
			}
		}

		if !eroareDetectata {
			fmt.Println("\n✅ Plan finalizat cu succes.")

			// Adăugăm în memorie doar dacă s-a terminat cu succes
			conversationHistory = append(conversationHistory, ChatMessage{Role: "user", Content: userInput})
			conversationHistory = append(conversationHistory, ChatMessage{
				Role:    "assistant",
				Content: "[Sistem: Plan executat. Modificările au fost salvate cu succes.]",
			})
		}
	}
}
