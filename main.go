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
	"strconv"
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
const configFileName = "autodev_config.json"

type Config struct {
	Provider     string `json:"provider"`      // "ollama", "vllm", "openai", "gemini"
	PlannerModel string `json:"planner_model"` // ex: "llama3.1:latest" sau "gpt-4o"
	CoderModel   string `json:"coder_model"`   // ex: "qwen2.5-coder:latest" sau "gpt-4o-mini"
	APIKey       string `json:"api_key"`       // Pentru cloud
	BaseURL      string `json:"base_url"`      // Pentru Ollama sau vLLM
}

// Încarcă setările sau returnează un default sigur
func loadConfig() *Config {
	cfg := &Config{
		Provider: "ollama",
		BaseURL:  "http://localhost:11434/api",
	}

	data, err := os.ReadFile(configFileName)
	if err == nil {
		json.Unmarshal(data, cfg)
	}
	return cfg
}

func saveConfig(cfg *Config) {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(configFileName, data, 0644)
}

type FileCache map[string]string

type PlanStep struct {
	Action       string   `json:"action"`                // "edit_file", "execute_command", "copy_code"
	FilePath     string   `json:"file_path"`             // Calea fișierului țintă
	SourceFile   string   `json:"source_file,omitempty"` // <-- NOU: Sursa de unde copiem
	Instructions string   `json:"instructions"`          // Instrucțiuni pentru Coder
	ReadFiles    []string `json:"read_files,omitempty"`  // <-- UNEALTA DE ENRICHMENT
}
type ExecutionPlan struct {
	Steps []PlanStep `json:"steps"`
}

// --- Funcții de Caching și Citire ---
func showSettingsMenu(cfg *Config) {
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Println("\n=== ⚙️  MENIU SETĂRI AUTODEV ===")
		fmt.Printf("1. Backend curent: %s\n", cfg.Provider)
		fmt.Printf("2. Model Planificator (Master): %s\n", cfg.PlannerModel)
		fmt.Printf("3. Model Coder (Worker): %s\n", cfg.CoderModel)
		fmt.Printf("4. API Key (Cloud): %s\n", func() string {
			if cfg.APIKey != "" {
				return "*** SETAT ***"
			}
			return "NESETAT"
		}())
		fmt.Println("5. Ieșire și Salvare")
		fmt.Print("\nCe dorești să modifici? (1-5): ")

		if !scanner.Scan() {
			return
		}
		choice := strings.TrimSpace(scanner.Text())

		switch choice {
		case "1":
			providers := []string{"ollama", "vllm", "openai", "gemini"}
			fmt.Println("\n🌐 Alege noul backend:")
			for i, p := range providers {
				fmt.Printf("  %d. %s\n", i+1, p)
			}
			fmt.Print("\n👉 Introdu numărul (1-4) sau scrie numele: ")

			if scanner.Scan() {
				input := strings.TrimSpace(scanner.Text())
				if input == "" {
					continue
				}

				// Verificăm dacă a introdus o cifră validă
				if idx, err := strconv.Atoi(input); err == nil && idx > 0 && idx <= len(providers) {
					cfg.Provider = providers[idx-1]
					fmt.Printf("✅ Backend setat la: %s\n", cfg.Provider)
				} else {
					// Verificăm dacă a scris direct numele provider-ului în loc de cifră
					inputLower := strings.ToLower(input)
					valid := false
					for _, p := range providers {
						if inputLower == p {
							cfg.Provider = p
							valid = true
							fmt.Printf("✅ Backend setat manual la: %s\n", cfg.Provider)
							break
						}
					}
					if !valid {
						fmt.Println("❌ Opțiune invalidă. Backend-ul a rămas neschimbat.")
					}
				}
			}
		case "2", "3":
			// Determinăm ce variabilă modificăm în funcție de alegere
			var target *string
			var roleName string
			if choice == "2" {
				target = &cfg.PlannerModel
				roleName = "Planificator (Master)"
			} else {
				target = &cfg.CoderModel
				roleName = "Coder (Worker)"
			}

			if cfg.Provider == "ollama" {
				// Dacă e Ollama, aducem modelele local
				modele, err := GetLocalModels()
				if err == nil && len(modele) > 0 {
					fmt.Printf("\n📦 Modele Ollama instalate pentru %s:\n", roleName)
					for i, m := range modele {
						fmt.Printf("  %d. %s\n", i+1, m)
					}
					fmt.Print("\n👉 Introdu numărul modelului (sau scrie manual numele): ")

					if scanner.Scan() {
						input := strings.TrimSpace(scanner.Text())
						if input == "" {
							continue
						}

						// Încercăm să convertim input-ul într-un număr (index)
						if idx, err := strconv.Atoi(input); err == nil && idx > 0 && idx <= len(modele) {
							*target = modele[idx-1] // Setăm modelul ales prin număr
							fmt.Printf("✅ Model %s setat la: %s\n", roleName, *target)
						} else {
							// Dacă nu e număr, presupunem că a scris manual numele modelului
							*target = input
							fmt.Printf("✅ Model %s setat manual la: %s\n", roleName, *target)
						}
					}
				} else {
					// Fallback dacă nu poate citi modelele
					fmt.Printf("Nu am putut citi modelele din Ollama. Introdu numele manual pentru %s: ", roleName)
					if scanner.Scan() {
						input := strings.TrimSpace(scanner.Text())
						if input != "" {
							*target = input
						}
					}
				}
			} else {
				// Dacă e cloud (OpenAI, Gemini), cerem pur și simplu text (ex: gpt-4o)
				fmt.Printf("Nume model %s (ex: gpt-4o, gemini-1.5-pro): ", roleName)
				if scanner.Scan() {
					input := strings.TrimSpace(scanner.Text())
					if input != "" {
						*target = input
					}
				}
			}

		case "4":
			fmt.Print("Introdu API Key (sau lasă gol pentru a anula): ")
			if scanner.Scan() {
				input := strings.TrimSpace(scanner.Text())
				if input != "" {
					cfg.APIKey = input
					fmt.Println("✅ API Key actualizat!")
				}
			}
		case "5":
			saveConfig(cfg)
			fmt.Println("💾 Setări salvate cu succes!")
			return
		default:
			fmt.Println("❌ Opțiune invalidă. Te rog alege o cifră de la 1 la 5.")
		}
	}
}
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
			Parameters:  ToolParameters{ /* ... rămâne la fel ... */ },
		},
	},
	{
		Type: "function",
		Function: ToolFunction{
			Name:        "toolExecuteCommand",
			Description: "Execută o comandă in terminal.",
			Parameters:  ToolParameters{ /* ... rămâne la fel ... */ },
		},
	},
	{
		Type: "function",
		Function: ToolFunction{
			Name:        "toolEditFile",
			Description: "Deleagă scrierea, crearea sau rescrierea unui fișier către AI-ul Coder Expert. Dacă fișierul nu există, sistemul îl va crea automat.",
			Parameters: ToolParameters{
				Type: "object",
				Properties: map[string]ToolProperty{
					"filePath":     {Type: "string", Description: "Calea către fișierul de modificat sau creat."},
					"instructions": {Type: "string", Description: "Explică ce cod trebuie scris de la zero sau ce modificare logică trebuie aplicată."},
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
	case "toolCopyCode":
		targetPath, _ := args["filePath"].(string)
		sourcePath, _ := args["sourceFile"].(string)
		instructions, _ := args["instructions"].(string)
		stepHistory, _ := args["stepHistory"].(string)

		if isFileProtected(targetPath) || isFileProtected(sourcePath) {
			return "Eroare: Nu poți interacționa cu fișiere protejate!"
		}

		// Citim fișierul sursă
		sourceContentBytes, err := os.ReadFile(sourcePath)
		if err != nil {
			return fmt.Sprintf("Eroare: Sursa '%s' nu există sau nu poate fi citită.", sourcePath)
		}

		// Citim / Creăm destinația
		targetExists := true
		targetContentBytes, err := os.ReadFile(targetPath)
		if err != nil {
			targetExists = false
			os.MkdirAll(filepath.Dir(targetPath), 0755)
		}
		targetContent := string(targetContentBytes)
		if !targetExists {
			targetContent = "// Fișier nou. Adaugă codul copiat aici."
		}

		approved, feedback := askUserConfirmation(fmt.Sprintf("Copiere/Mutare din `%s` în `%s`\n   Instrucțiuni: %s", sourcePath, targetPath, instructions))
		if !approved {
			return feedback
		}

		prompt := fmt.Sprintf(`Ești un expert în integrarea codului.
Ai o memorie a pașilor:
%s

Ai misiunea de a TRANSFERA cod conform instrucțiunilor.
================= SURSĂ (%s) (De aici extragi) =================
%s
==================================================================
================= DESTINAȚIE (%s) (Aici inserezi/integrezi) ====
%s
==================================================================

INSTRUCȚIUNI DE TRANSFER:
%s

REGULI ABSOLUTE:
1. Returnează DOAR codul final COMPLET pentru fișierul DESTINAȚIE (%s).
2. Nu trunchia codul. Scrie-l complet și corect.
`, stepHistory, sourcePath, string(sourceContentBytes), targetPath, targetContent, instructions, targetPath)

		resp, err := CallOllamaGenerate(coderModel, prompt)
		if err != nil {
			return fmt.Sprintf("Eroare la generare: %v", err)
		}
		cleanCode := extractCodeFromMarkdown(resp)

		// Salvăm direct
		if err := os.WriteFile(targetPath, []byte(cleanCode), 0644); err != nil {
			return fmt.Sprintf("Eroare la salvare: %v", err)
		}
		return fmt.Sprintf("✅ Codul a fost integrat cu succes în %s de către %s.", targetPath, coderModel)
	case "toolEditFile":
		path, _ := args["filePath"].(string)
		instructions, _ := args["instructions"].(string)
		referenceContext, _ := args["referenceContext"].(string)
		stepHistory, _ := args["stepHistory"].(string) // Extragem Memoria!
		if isFileProtected(path) {
			return "Eroare: Acces interzis. Nu poți modifica fișiere de sistem sau biblioteci externe!"
		}

		// 1. Verificăm dacă fișierul există
		fileExists := true
		currentContentBytes, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				fileExists = false
			} else {
				return fmt.Sprintf("Eroare la citirea fișierului: %v", err)
			}
		}

		actionName := "Modificare"
		if !fileExists {
			actionName = "Creare"
		}

		approved, feedback := askUserConfirmation(fmt.Sprintf("%s fișier: `%s`\n   Instrucțiuni: %s", actionName, path, instructions))
		if !approved {
			return feedback
		}

		currentContent := string(currentContentBytes)
		if !fileExists {
			currentContent = "// Acest fișier este nou și momentan este gol. Scrie codul de la zero conform cerinței."
		}

		fmt.Printf("\n[Sub-Agent] 🧠 Apelez %s pentru a %s fișierul...\n", coderModel, strings.ToLower(actionName))

		refBlock := ""
		if referenceContext != "" {
			refBlock = fmt.Sprintf(`
================= CONTEXT REFERINȚĂ (NU MODIFICA) =================
Aceste fișiere au fost deja modificate la pașii anteriori. 
Privește-le DOAR pentru a ști ce clase, ID-uri sau funcții să folosești.
ESTE INTERZIS SĂ RETURNEZI COD PENTRU ACESTE FIȘIERE!
%s
===================================================================
`, referenceContext)
		}

		prompt := fmt.Sprintf(`Ești un expert programator.
Ai o memorie a pașilor executați anterior pentru a înțelege contextul:
%s

%s
================= FIȘIER ȚINTĂ (%s) =================
%s
==================================================================

CERINȚĂ PENTRU ACEST PAS:
%s

REGULI ABSOLUTE:
1. Returnează codul COMPLET și EXACT DOAR pentru fișierul țintă. FĂRĂ EXPLICAȚII.
2. NU folosi "// restul codului". Scrie cap-coadă.
`, stepHistory, refBlock, path, currentContent, instructions)

		resp, err := CallOllamaGenerate(coderModel, prompt)
		if err != nil {
			return fmt.Sprintf("Eroare la generarea codului cu %s: %v", coderModel, err)
		}

		cleanCode := extractCodeFromMarkdown(resp)

		// 2. VALIDARE ANTI-TRUNCHIERE (SAFETY CHECK)
		lazyKeywords := []string{"// ...", "/* ... */", "// restul codului", "// rest of"}
		for _, kw := range lazyKeywords {
			if strings.Contains(strings.ToLower(cleanCode), kw) {
				return "Eroare: AI-ul a trunchiat codul (a folosit comentarii în loc să scrie tot codul). Modificarea a fost anulată pentru siguranță."
			}
		}

		// Verificăm dimensiunea DOAR dacă fișierul exista deja
		if fileExists && len(currentContent) > 0 {
			if len(cleanCode) < int(float64(len(currentContent))*0.2) {
				return "Eroare: Codul generat este suspect de mic față de original. Pentru a preveni ștergerea fișierului, acțiunea a fost blocată."
			}

			// SISTEM DE BACKUP AUTOMAT
			backupPath := path + ".bak"
			err = os.WriteFile(backupPath, currentContentBytes, 0644)
			if err == nil {
				fmt.Printf("📦 Backup creat la: %s\n", backupPath)
			}
		}

		// 3. Creăm folderele necesare dacă nu există (vital pentru fișiere noi)
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Sprintf("Eroare la crearea directoarelor pentru %s: %v", path, err)
		}

		// 4. SCRIEREA FINALĂ
		if err := os.WriteFile(path, []byte(cleanCode), 0644); err != nil {
			return fmt.Sprintf("Eroare la salvarea fișierului: %v", err)
		}

		statusMsg := "rescris"
		if !fileExists {
			statusMsg = "creat"
		}
		return fmt.Sprintf("Fișierul a fost %s cu succes de %s.", statusMsg, coderModel)
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

	// 1. PLASA DE SIGURANȚĂ EXTINSĂ (cuvinte cheie care indică clar o acțiune)
	actionKeywords := []string{
		"ruleaza", "rulează", "test", "executa", "porneste", "pornește", "run", "go run", "testeaza",
		"creeaza", "creează", "modifica", "modifică", "adauga", "adaugă", "repara", "repară",
		"scrie", "implementeaza", "implementează", "sterge", "șterge", "bug", "eroare",
		"schimba", "schimbă", "refactorizeaza", "actualizeaza", "fa un", "fă un", "genereaza", "editeaza",
	}

	for _, kw := range actionKeywords {
		// Folosim o verificare simplă, dar eficientă
		if strings.Contains(userInputLower, kw) {
			return "CODE"
		}
	}

	// 2. PROMPT STRICT DE TIP "FEW-SHOT" (cu exemple incluse)
	systemPrompt := `Ești un router de intenții precis. Analizează cerința utilizatorului și răspunde DOAR cu 'CODE' sau 'CHAT'.

REGULI:
- Răspunde 'CODE' dacă utilizatorul cere o acțiune tehnică: să creeze, să modifice, să scrie cod, să șteargă, să repare, să ruleze sau să testeze.
- Răspunde 'CHAT' DOAR dacă este o întrebare teoretică, o cerere de explicație generală, sau o discuție liberă.

EXEMPLE:
Utilizator: "ce este o interfață în Go?" -> CHAT
Utilizator: "adaugă o funcție de login în main.go" -> CODE
Utilizator: "cum funcționează HTTP?" -> CHAT
Utilizator: "am o eroare la linia 20, rezolv-o" -> CODE
Utilizator: "creează un fișier nou index.html" -> CODE
Utilizator: "ce mai faci?" -> CHAT
`

	reqBody := ChatRequest{
		Model: modelName,
		Messages: []ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userInput},
		},
		Stream: false,
		// 3. SCĂDEREA TEMPERATURII pentru a evita răspunsuri neașteptate
		Options: map[string]interface{}{
			"temperature": 0.1,
		},
	}

	jsonData, _ := json.Marshal(reqBody)
	resp, err := http.Post(ollamaBaseURL+"/chat", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return "CHAT" // Fallback la chat în caz de eroare de rețea
	}
	defer resp.Body.Close()

	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "CHAT"
	}

	// Extragem și curățăm răspunsul
	intent := strings.TrimSpace(strings.ToUpper(chatResp.Message.Content))

	// Verificăm strict rezultatul pentru a evita halucinațiile
	if strings.Contains(intent, "CODE") {
		return "CODE"
	}
	return "CHAT"
}

func generateExecutionPlan(plannerModel, userInput, projectTree string, persistentDocs string) (*ExecutionPlan, error) {
	prompt := fmt.Sprintf(`Ești arhitectul strict al unui sistem AI. 
Scopul tău este să generezi un JSON cu pași, analizând harta de fișiere de mai jos.

ARBORELE FIȘIERELOR EXISTENTE:
===========================
%s
===========================
%s

CERINȚA UTILIZATORULUI: "%s"

// Găsește secțiunea REGULI ABSOLUTE DE PLANIFICARE și înlocuiește-o cu asta:

REGULI ABSOLUTE DE PLANIFICARE (CITEȘTE CU ATENȚIE):
1. REGULA PENTRU RULARE: Pentru a testa sau rula, folosește DOAR "execute_command".
2. REGULA PENTRU FIȘIERE (CREARE/EDITARE): Folosește "edit_file" pentru a scrie de la zero sau a modifica un fișier.
3. UNEALTA DE COPIERE/MUTARE (copy_code): Dacă trebuie să transferi, copiezi sau muți cod dintr-un fișier în altul, folosește acțiunea "copy_code". Setează "source_file" cu fișierul de unde iei codul și "file_path" cu destinația unde îl pui.
4. UNEALTA DE CONTEXT ENRICHMENT (read_files): Dacă ai nevoie ca AI-ul Coder să "citească" un fișier pentru a-i înțelege funcțiile, adaugă acel fișier în lista "read_files".
5. CÂMPURI OBLIGATORII: "action", "file_path" și "instructions" sunt obligatorii.

EXEMPLU - COPIERE / MUTARE COD:
{
  "steps": [
    {
      "action": "copy_code",
      "source_file": "vechi.go",
      "file_path": "nou.go",
      "instructions": "Extrage functia de Login din fisierul sursa si adapteaz-o in fisierul destinatie. Daca e o mutare, scrie la final sa comenteze/stearga vechea functie."
    }
  ]
}
EXEMPLU 2 - Editare cu Context (Folosire stiluri, referințe, DOAR CITIRE):
{
  "steps": [
    {
      "action": "edit_file",
      "file_path": "contact.html",
      "instructions": "Infrumuseteaza pagina contact.html folosind clasele din fisierul CSS atasat.",
      "read_files": ["style.css"]
    }
  ]
}

`, projectTree, persistentDocs, userInput)
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
	fmt.Println(chatResp)

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

const historyFileName = ".autodev_history.json"

// Încarcă istoricul conversației din fișier (dacă există)
func loadHistory() []ChatMessage {
	var history []ChatMessage
	data, err := os.ReadFile(historyFileName)
	if err == nil {
		json.Unmarshal(data, &history)
	}
	return history
}

// Salvează istoricul curent în fișier
func saveHistory(history []ChatMessage) {
	data, err := json.MarshalIndent(history, "", "  ")
	if err == nil {
		os.WriteFile(historyFileName, data, 0644)
	}
}

// analyzeErrorAndSuggest scanează logul de eroare și generează sugestii pentru AI
func analyzeErrorAndSuggest(errorLog string, failedCmd string) string {
	var hints strings.Builder
	errorLogLower := strings.ToLower(errorLog)
	cmdLower := strings.ToLower(failedCmd)

	hints.WriteString("🔍 ANALIZĂ AUTOMATĂ ȘI SUGESTII DE REPARARE PENTRU AI:\n")

	// 1. Detectare Python
	if strings.Contains(cmdLower, "python") || strings.Contains(cmdLower, "pip") || strings.Contains(cmdLower, ".py") || strings.Contains(errorLogLower, "traceback (most recent call last)") {
		hints.WriteString("- Limbaj detectat: Python 🐍\n")
		if strings.Contains(errorLogLower, "modulenotfounderror") || strings.Contains(errorLogLower, "no module named") {
			hints.WriteString("- 💡 Diagnostic: O bibliotecă lipsește.\n- 🛠️ Acțiune recomandată: Adaugă un prim pas de tip 'execute_command' pentru a rula `pip install <nume_pachet>` (ex: `pip install requests`), apoi un al doilea pas pentru a rula din nou scriptul.\n")
		} else if strings.Contains(errorLogLower, "syntaxerror") || strings.Contains(errorLogLower, "indentationerror") {
			hints.WriteString("- 💡 Diagnostic: Eroare de sintaxă sau indentare.\n- 🛠️ Acțiune recomandată: Folosește 'edit_file' pentru a corecta linia exactă menționată în Traceback.\n")
		} else {
			hints.WriteString("- 🛠️ Comenzi utile de referință: `python ./cale/script.py`, `pip install pachet`.\n")
		}
	}

	// 2. Detectare Go
	if strings.Contains(cmdLower, "go ") || strings.HasSuffix(cmdLower, ".go") || strings.Contains(errorLogLower, "goroutine") || strings.Contains(errorLogLower, "build failed") {
		hints.WriteString("- Limbaj detectat: Go 🐹\n")
		if strings.Contains(errorLogLower, "cannot find package") || strings.Contains(errorLogLower, "no required module provides") {
			hints.WriteString("- 💡 Diagnostic: Pachet extern lipsă.\n- 🛠️ Acțiune recomandată: Adaugă un pas 'execute_command' cu `go mod tidy` sau `go get <cale_pachet>` înainte de a rula din nou.\n")
		} else if strings.Contains(errorLogLower, "undefined:") {
			hints.WriteString("- 💡 Diagnostic: Variabilă, funcție sau import nedefinit.\n- 🛠️ Acțiune recomandată: Folosește 'edit_file' pentru a verifica importurile și a repara denumirile din fișier.\n")
		}
	}

	// 3. Detectare Node.js / JavaScript
	if strings.Contains(cmdLower, "node ") || strings.Contains(cmdLower, "npm ") || strings.HasSuffix(cmdLower, ".js") || strings.Contains(errorLogLower, "referenceerror") {
		hints.WriteString("- Limbaj detectat: JavaScript / Node.js 🟨\n")
		if strings.Contains(errorLogLower, "cannot find module") {
			hints.WriteString("- 💡 Diagnostic: Modul NPM lipsă.\n- 🛠️ Acțiune recomandată: Adaugă un pas 'execute_command' cu `npm install <nume_pachet>`.\n")
		}
	}

	hints.WriteString("\n🧠 CUM SĂ GÂNDEȘTI ACEASTĂ REPARARE (Chain of Thought):\n")
	hints.WriteString("Pasul A: Identifică din eroare CARE este fișierul vinovat sau pachetul lipsă.\n")
	hints.WriteString("Pasul B: Gândește-te dacă trebuie să editezi codul (edit_file) SAU să instalezi ceva în terminal (execute_command).\n")
	hints.WriteString("Pasul C: Creează noul plan JSON aplicând soluția.\n")

	return hints.String()
}
func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("Eroare: %v\n", err)
		return
	}

	// --- 1. ÎNCĂRCARE SETĂRI ---
	cfg := loadConfig()

	// Dacă suntem pe Ollama și nu avem modele setate, încercăm să auto-detectăm
	if cfg.Provider == "ollama" && (cfg.PlannerModel == "" || cfg.CoderModel == "") {
		modele, err := GetLocalModels()
		if err == nil && len(modele) > 0 {
			for _, m := range modele {
				if strings.Contains(strings.ToLower(m), "llama") && cfg.PlannerModel == "" {
					cfg.PlannerModel = m
				}
				if strings.Contains(strings.ToLower(m), "qwen") && cfg.CoderModel == "" {
					cfg.CoderModel = m
				}
			}
			if cfg.PlannerModel == "" {
				cfg.PlannerModel = modele[0]
			}
			if cfg.CoderModel == "" {
				cfg.CoderModel = modele[0]
			}
			saveConfig(cfg) // Salvăm ce am găsit
		}
	}

	fmt.Println("\n====================================================")
	fmt.Println("🌟 AUTODEV: ARHITECTURĂ MASTER-WORKER ACTIVATĂ 🌟")
	fmt.Printf("🌐 Provider: %s\n", strings.ToUpper(cfg.Provider))
	if cfg.Provider == "ollama" {
		fmt.Printf("🧠 Planificator: %s\n", cfg.PlannerModel)
		fmt.Printf("💻 Executant: %s\n", cfg.CoderModel)
	} else {
		fmt.Printf("🧠 Model Cloud: %s\n", cfg.PlannerModel)
	}
	fmt.Println("Tastați '/setari' pentru a modifica backend-ul.")
	fmt.Println("Tastați '/clear' pentru a sterge istoricul.")
	fmt.Println("====================================================")
	scanner := bufio.NewScanner(os.Stdin)

	// --- MODIFICAT: Încărcăm memoria salvată anterior ---
	conversationHistory := loadHistory()
	if len(conversationHistory) > 0 {
		fmt.Printf("📂 [Sistem] Am încărcat %d mesaje anterioare din memorie.\n", len(conversationHistory))
	}
	var autoPrompt string
	var persistentDocs string // <--- NOU: Aici vom stoca documentația de proiect

	for {
		// --- 2. SCANARE PROASPĂTĂ ---
		cache := loadCache(cwd)
		newCache := make(FileCache)

		var treeBuilder strings.Builder
		var contextBuilder strings.Builder // Păstrăm și conținutul pentru Coder (dacă e nevoie la creare)

		treeBuilder.WriteString("Căi relative disponibile în proiect:\n")

		filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}

			if info.IsDir() {
				n := info.Name()
				if n == "node_modules" || n == ".git" || n == ".venv" || n == "venv" || n == "__pycache__" {
					return filepath.SkipDir
				}
				return nil
			}

			relPath, _ := filepath.Rel(cwd, path)
			if relPath != cacheFileName && !strings.HasPrefix(info.Name(), "autodev") {

				textContent, _ := getContent(path)
				currentHash := computeHash([]byte(textContent))
				newCache[relPath] = currentHash

				oldHash, exists := cache[relPath]
				if !exists || oldHash != currentHash {
					fmt.Printf("[Detectat/Modificat] 📄 %s\n", relPath)
				}

				treeBuilder.WriteString(fmt.Sprintf("  - %s\n", relPath))
				contextBuilder.WriteString(fmt.Sprintf("\n--- Fișier: %s ---\n%s\n", relPath, textContent))
			}
			return nil
		})
		saveCache(cwd, newCache)

		projectTree := treeBuilder.String()

		// --- 3. CITIRE INPUT SAU AUTO-PROMPT ---
		var userInput string
		if autoPrompt != "" {
			userInput = autoPrompt
			fmt.Println("\n🔄 [Auto-Recuperare] Trimit automat eroarea către AI...")
			autoPrompt = ""
		} else {
			fmt.Print("\n> Cerința ta: ")
			if !scanner.Scan() {
				break
			}
			userInput = scanner.Text()
		}

		if strings.ToLower(userInput) == "/exit" {
			break
		}
		if strings.HasPrefix(strings.ToLower(userInput), "/getcontext") {
			parts := strings.SplitN(userInput, " ", 2)
			if len(parts) < 2 {
				fmt.Println("⚠️ Te rog să specifici calea către fișier. Exemplu: /getcontext docs/specificatii.md")
				continue
			}

			docPath := strings.TrimSpace(parts[1])
			content, err := getContent(docPath) // Folosim funcția ta magică care citește și PDF/Docx!

			if err != nil {
				fmt.Printf("❌ Eroare la citirea documentației '%s': %v\n", docPath, err)
			} else {
				persistentDocs = fmt.Sprintf("\n================= DOCUMENTAȚIE / SPECIFICAȚII PROIECT =================\n%s\n=======================================================================\n", content)
				fmt.Printf("✅ Documentația din '%s' a fost încărcată cu succes în memoria permanentă!\n", docPath)
			}
			continue
		}
		if strings.ToLower(userInput) == "/setari" {
			showSettingsMenu(cfg)
			continue
		}
		if strings.ToLower(userInput) == "/clear" || strings.ToLower(userInput) == "/curata" {
			conversationHistory = nil
			os.Remove(historyFileName) // Ștergem și fișierul fizic
			fmt.Println("\n🧹 [Sistem] Memoria conversației a fost ștearsă cu succes! O luăm de la capăt.")
			continue
		}
		if strings.TrimSpace(userInput) == "" {
			continue
		}

		// =================================================================
		// RAMURA PENTRU MODELE LOCALE (OLLAMA)
		// =================================================================
		if cfg.Provider == "ollama" {

			// DOAR pentru Ollama facem clasificarea intenției
			intent := classifyIntent(userInput, cfg.PlannerModel)
			fmt.Printf("[Sistem Ollama] Intenție detectată: %s\n", intent)

			if intent == "CHAT" {
				systemContext := fmt.Sprintf(`Ești un asistent programator.
Ai la dispoziție următoarea structură a proiectului curent:
%s
%s

REGULI ABSOLUTE PENTRU COMPORTAMENT ȘI UNELTE:
1. Dacă întrebarea este despre codul tău existent, FOLOSEȘTE 'toolOpenFile' DOAR pentru fișierele afișate exact în lista de mai sus.
2. ESTE STRICT INTERZIS să inventezi nume de fișiere sau căi (ex: pointer.md, test.cpp) care nu sunt în listă.
3. Dacă utilizatorul pune o întrebare teoretică, generală sau de sintaxă (ex: "ce este un pointer?", "cum fac un for?"), RĂSPUNDE DIRECT pe bază de text, FĂRĂ să apelezi niciun tool!
4. Nu ghici conținutul fișierelor proiectului, citește-le prin tool dacă e nevoie de ele.`, projectTree, persistentDocs)

				mesajeChat := append(conversationHistory, ChatMessage{Role: "user", Content: userInput})
				mesajeChat = append([]ChatMessage{{Role: "system", Content: systemContext}}, mesajeChat...)

				responseMsg, err := CallOllamaChat(cfg.PlannerModel, mesajeChat, autoDevTools)
				if err != nil {
					fmt.Println("\n❌ [Eroare CHAT]:", err)
					continue
				}

				if len(responseMsg.ToolCalls) > 0 {
					for _, tc := range responseMsg.ToolCalls {
						if tc.Function.Name == "toolOpenFile" {
							filePath, _ := tc.Function.Arguments["filePath"].(string)
							fmt.Printf("\n🔍 [Asistentul folosește un Tool: Citește '%s']...\n", filePath)

							toolResult := handleToolCall(tc.Function.Name, tc.Function.Arguments, "", "")

							mesajeChat = append(mesajeChat, responseMsg)
							mesajeChat = append(mesajeChat, ChatMessage{
								Role:    "tool",
								Content: toolResult,
							})
						}
					}

					finalResponse, err := CallOllamaChat(cfg.PlannerModel, mesajeChat, nil)
					if err == nil {
						fmt.Println("\n🗣️ [Asistent]:", finalResponse.Content)
						conversationHistory = append(conversationHistory, ChatMessage{Role: "user", Content: userInput}, finalResponse)
						saveHistory(conversationHistory)
					}
				} else {
					fmt.Println("\n🗣️ [Asistent]:", responseMsg.Content)
					conversationHistory = append(conversationHistory, ChatMessage{Role: "user", Content: userInput}, responseMsg)
					saveHistory(conversationHistory)
				}
				continue // Ne întoarcem la începutul buclei, am terminat cu CHAT-ul
			}

			// Dacă a ajuns aici, înseamnă că intent == "CODE"
			fmt.Printf("\n[Planificator] 🧠 `%s` analizează cerința și creează planul de acțiune...\n", cfg.PlannerModel)

			plan, err := generateExecutionPlan(cfg.PlannerModel, userInput, projectTree, persistentDocs)
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
				if strings.TrimSpace(step.Instructions) == "" {
					continue
				}
				fmt.Printf("  Pas %d: [%s] -> %s\n", i+1, step.Action, step.Instructions)
			}
			fmt.Println("----------------------------------------------------")

			fisiereModificateContext := make(map[string]string)
			eroareDetectata := false

			// ---> NOU: Inițializăm Jurnalul de Memorie al Planului
			var istoricPasi strings.Builder
			istoricPasi.WriteString("CE AI FĂCUT PÂNĂ ACUM ÎN ACEST PLAN:\n")

			for i, step := range plan.Steps {
				if strings.TrimSpace(step.Instructions) == "" {
					continue
				}
				fmt.Printf("\n🚀 Execut Pasul %d: %s\n", i+1, step.Action)

				var toolResult string
				args := make(map[string]interface{})

				// --- PREGĂTIRE MEMORIE (PLAN + PAS) ---
				var contextReferinta strings.Builder

				// A. Memoria de Plan (Fișiere deja modificate în pașii anteriori)
				if len(fisiereModificateContext) > 0 {
					for fpath, fcontent := range fisiereModificateContext {
						contextReferinta.WriteString(fmt.Sprintf("\n[MEMORIA DE PLAN - Modificat Anterior: %s]\n```\n%s\n```\n", fpath, fcontent))
					}
				}

				// B. Memoria de Pas (Context Enrichment cerut de Planificator)
				if len(step.ReadFiles) > 0 {
					for _, readFile := range step.ReadFiles {

						// --- 1. SMART PATH RESOLVER (Găsim calea reală) ---
						actualPath := readFile
						if _, err := os.Stat(actualPath); os.IsNotExist(err) {
							// Dacă nu există direct, îl căutăm după nume în tot proiectul
							filepath.Walk(".", func(p string, info os.FileInfo, err error) error {
								if err == nil && !info.IsDir() && info.Name() == filepath.Base(readFile) {
									actualPath = p // Am găsit fișierul ascuns într-un folder!
								}
								return nil
							})
						}

						// --- 2. CĂUTĂM ÎN MEMORIA DE PLAN (Fișiere abia modificate) ---
						foundInMemory := false
						for fpath := range fisiereModificateContext {
							// Comparație flexibilă (ex: frontend/search.js se potrivește cu search.js)
							if filepath.Base(fpath) == filepath.Base(actualPath) {
								// ESTE DEJA ÎN CONTEXT DIN BLOCUL A! Nu mai printăm conținutul din nou în `contextReferinta`.
								fmt.Printf("   📚 [Enrichment] '%s' a fost cerut, dar este DEJA pus pe masă din Memoria de Plan (evităm duplicarea).\n", actualPath)
								foundInMemory = true
								break
							}
						}

						// --- 3. CĂUTĂM PE DISC (Dacă nu e deja în memorie) ---
						if !foundInMemory {
							fileContent, err := os.ReadFile(actualPath)
							if err == nil {
								contextReferinta.WriteString(fmt.Sprintf("\n[MEMORIA DE PAS - Sursă Citire: %s]\n```\n%s\n```\n", actualPath, string(fileContent)))
								fmt.Printf("   📚 [Enrichment] Am atașat context de pe DISC din: %s\n", actualPath)
							} else {
								fmt.Printf("   ⚠️ [Avertisment] Nu am putut găsi fișierul de context absolut deloc: %s\n", readFile)
							}
						}
					}
				}
				args["referenceContext"] = contextReferinta.String()
				args["stepHistory"] = istoricPasi.String() // AI-ul primeste istoria
				args["referenceContext"] = contextReferinta.String()

				switch step.Action {
				case "copy_code": // ---> NOU: Executăm tool-ul de copiere
					args["filePath"] = step.FilePath
					args["sourceFile"] = step.SourceFile
					args["instructions"] = step.Instructions
					toolResult = handleToolCall("toolCopyCode", args, cfg.CoderModel, contextBuilder.String())
				case "edit_file": // <--- Acum preia și crearea și editarea!
					args["filePath"] = step.FilePath
					args["instructions"] = step.Instructions
					toolResult = handleToolCall("toolEditFile", args, cfg.CoderModel, contextBuilder.String())
				case "execute_command":
					args["command"] = step.Instructions
					toolResult = handleToolCall("toolExecuteCommand", args, cfg.CoderModel, contextBuilder.String())
				default:
					toolResult = fmt.Sprintf("Acțiune necunoscută: %s", step.Action)
				}

				fmt.Printf("[Rezultat Pas %d]:\n%s\n", i+1, toolResult)

				if strings.Contains(toolResult, "OPRIT") || strings.Contains(toolResult, "respins") {
					fmt.Println("\n⚠️ Execuția planului a fost întreruptă de utilizator.")
					eroareDetectata = true
					break
				}

				if strings.Contains(toolResult, "Eroare") || strings.Contains(toolResult, "error") || strings.Contains(toolResult, "Exception") || strings.Contains(toolResult, "Traceback") {
					fmt.Printf("\n⚠️ Eroare detectată la Pasul %d! Pregătesc auto-repararea...\n", i+1)

					autoPrompt = fmt.Sprintf(`A apărut o EROARE CRITICĂ la executarea ultimului pas:
=========================================
%s
=========================================

SARCINĂ STRICTĂ DE AUTO-REPARARE:
1. CITEȘTE TRACEBACK-UL: Analizează cu atenție log-ul de eroare de mai sus. Numele fișierului real care a crăpat este scris explicit acolo (ex: File "main.py", line X).
2. FĂRĂ HALUCINAȚII: NU inventa fișiere noi (ex: nu crea un fișier doar pentru că o funcție are acel nume). Uită-te în ARBORELE FIȘIERELOR EXISTENTE și folosește fișierul real.
3. REPARĂ: Creează un plan NOU cu acțiunea "edit_file" EXACT pe fișierul vinovat pentru a repara eroarea logică.
4. TESTEAZĂ: La finalul planului, adaugă OBLIGATORIU un pas de "execute_command" pentru a rula aplicația din nou și a verifica dacă s-a reparat.`, toolResult)

					eroareDetectata = true
					break
				}

				if step.Action == "edit_file" {
					newContentBytes, err := os.ReadFile(step.FilePath)
					if err == nil {
						fisiereModificateContext[step.FilePath] = string(newContentBytes)
					}
				}
			}

			if !eroareDetectata {
				fmt.Println("\n✅ Plan finalizat cu succes.")
				conversationHistory = append(conversationHistory, ChatMessage{Role: "user", Content: userInput})
				conversationHistory = append(conversationHistory, ChatMessage{
					Role:    "assistant",
					Content: "[Sistem: Plan executat cu succes folosind modele locale.]",
				})
				saveHistory(conversationHistory)
			}

		} else {
			// =================================================================
			// RAMURA PENTRU MODELE CLOUD (OpenAI / Gemini / Anthropic)
			// =================================================================
			fmt.Printf("\n☁️ [Cloud Agent] Apelez %s (%s) pentru a rezolva cerința...\n", strings.ToUpper(cfg.Provider), cfg.PlannerModel)

			// Pentru modelele Cloud, nu ne complicăm cu JSON planificator.
			// Cloud-ul primește promptul cu instrucțiuni și lista de Tools (funcții)
			// și decide singur ce să apeleze.

			/* Exemplu logic de viitor pentru Cloud:

			mesaje := append(conversationHistory, ChatMessage{Role: "user", Content: userInput})
			response, _ := CallCloudModel(cfg, mesaje, autoDevTools)

			// Buclă de Tools:
			dacă response are ToolCalls:
				execută handleToolCall pentru fiecare (creare fisier, rulare comanda etc.)
				atașează rezultatele la 'mesaje'
				CallCloudModel din nou
			altfel:
				afișează răspunsul (conversație)
			*/

			fmt.Println("⚠️ Logica de execuție pentru Cloud este în curs de implementare.")
		}
	}
}
