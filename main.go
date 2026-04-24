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
	"regexp"
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
Ai primit sarcina să modifici ACEST fișier:
<file_to_edit path="%s">
%s
</file_to_edit>

CERINȚĂ:
%s

REGULI DE EDITARE:
- Pentru a MODIFICA: Pune codul vechi în <search> și cel nou în <replace>.
- Pentru a ADĂUGA o metodă/funcție nouă într-o CLASĂ existentă: NU lăsa <search> gol! Pune ultima metodă din acea clasă în <search> și scrie-o din nou în <replace> urmată imediat de metoda ta nouă.
- Pentru a ADĂUGA funcții globale / independente: Lasă <search></search> complet gol, iar sistemul le va insera corect la final.
- Pentru a adăuga în interiorul funcției main(): NU lăsa search gol. Pune ultima linie din main() în <search> și rescrie finalul funcției în <replace>.
<diff>
  <search>
  codul vechi care trebuie înlocuit
  </search>
  <replace>
  noul cod care vine în locul celui vechi
  </replace>
</diff>

Poți folosi mai multe blocuri <diff> dacă trebuie să modifici în mai multe locuri. Răspunde DOAR cu XML-ul, fără explicații suplimentare.
`, stepHistory, refBlock, path, currentContent, instructions)

		resp, err := CallOllamaGenerate(coderModel, prompt)
		if err != nil {
			return fmt.Sprintf("Eroare la generarea codului cu %s: %v", coderModel, err)
		}
		fmt.Println("\n================= DEBUG: RAW CODER RESPONSE =================")
		fmt.Println(resp)
		fmt.Println("=============================================================")
		cleanCode := extractCodeFromMarkdown(resp)

		// Dacă fișierul e NOU, acceptăm pur și simplu codul (fără search/replace)
		if !fileExists {
			cleanCode = extractCodeFromMarkdown(resp) // sau o funcție care extrage din <replace>
			cleanCode = cleanXMLTags(cleanCode)
		} else {
			// Dacă fișierul EXISTĂ, aplicăm logica de DIFF XML
			newContent, errDiff := applyXMLDiff(currentContent, resp, path) // <--- Adaugă path aici
			if errDiff != nil {
				return fmt.Sprintf("Eroare la aplicarea modificărilor (diff a eșuat): %v\nRăspuns brut: %s", errDiff, resp)
			}
			cleanCode = newContent

			// SISTEM DE BACKUP AUTOMAT (păstrăm ce aveai deja)
			backupPath := path + ".bak"
			err = os.WriteFile(backupPath, currentContentBytes, 0644)
			if err == nil {
				fmt.Printf("📦 Backup creat la: %s\n", backupPath)
			}
		}

		// Creăm folderele necesare
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Sprintf("Eroare la crearea directoarelor pentru %s: %v", path, err)
		}

		// SCRIEREA FINALĂ
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
	// =========================================================================
	// PASUL 1: GENERAREA PRIMULUI PLAN (DRAFT)
	// =========================================================================
	prompt := fmt.Sprintf(`Ești arhitectul strict al unui sistem AI. 
Scopul tău este să generezi un JSON cu pași, analizând harta de fișiere de mai jos.

ARBORELE FIȘIERELOR EXISTENTE:
===========================
%s
===========================
%s

CERINȚA UTILIZATORULUI: "%s"

REGULI ABSOLUTE DE PLANIFICARE (CITEȘTE CU ATENȚIE):
1. STRUCTURA DATELOR:
   - "action" POATE FI DOAR UNA DIN URMĂTOARELE 3 VALORI STRICE: "edit_file", "execute_command", "copy_code". NU INVENTA acțiuni noi.
   - "file_path" ESTE STRICT UN STRING (o singură cale de fișier), NICIODATĂ O LISTĂ.
   - "read_files" este STRICT O LISTĂ de stringuri (ex: ["fisier1.py", "fisier2.md"]) și se folosește OBLIGATORIU dacă asistentul are nevoie de detalii, clase, ID-uri sau endpoint-uri din alte fișiere pentru a scrie codul corect.

2. LOGICA DE EXECUTIE PENTRU SCRIPTURI: Dacă utilizatorul cere să procesezi fișiere complexe, TREBUIE să planifici crearea unui script prin "edit_file", urmată de rularea lui prin "execute_command".

EXEMPLU EDITARE CU ENRICHMENT (CUM CITEȘTI CONTEXTUL):
{
  "steps": [
    {
      "action": "edit_file",
      "file_path": "frontend.js",
      "instructions": "Folosește endpoint-urile descrise în server.py pentru a face fetch.",
      "read_files": ["server.py"] 
    }
  ]
}

EXEMPLU PENTRU SCRIPTURI:
{
  "steps": [
    {
      "action": "edit_file",
      "file_path": "procesare.py",
      "instructions": "Scrie un script..."
    },
    {
      "action": "execute_command",
      "file_path": "",
      "instructions": "python procesare.py"
    }
  ]
}

REGULĂ VITALĂ: Exemplele de mai sus sunt DOAR DEMONSTRATIVE. Este STRICT INTERZIS să copiezi numele fișierelor din exemple. Creează un plan ORIGINAL.

REȚINE: Returnează DOAR cod JSON valid, fără text suplimentar.
`, projectTree, persistentDocs, userInput)

	// Cererea 1: Către Ollama
	reqBody1 := ChatRequest{
		Model: plannerModel,
		Messages: []ChatMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
		Options: map[string]interface{}{
			"temperature": 0.1,
		},
	}

	jsonData1, _ := json.Marshal(reqBody1)
	resp1, err := http.Post(ollamaBaseURL+"/chat", "application/json", bytes.NewBuffer(jsonData1))
	if err != nil {
		return nil, err
	}
	defer resp1.Body.Close()

	body1, _ := io.ReadAll(resp1.Body)
	var chatResp1 ChatResponse
	json.Unmarshal(body1, &chatResp1)

	draftJSONStr, found := extractJSONFromText(chatResp1.Message.Content)
	if !found {
		draftJSONStr = chatResp1.Message.Content
	}

	fmt.Printf("\n[Reflecție Internă] 🧐 Am generat planul draft. Verific dacă folosește enrichment (read_files)...\n")

	// =========================================================================
	// PASUL 2: REFLECȚIA ȘI CORECTAREA (SELF-CORRECTION)
	// =========================================================================

	reflectionPrompt := fmt.Sprintf(`Acesta este planul JSON pe care l-ai generat pentru a rezolva cerința: "%s"

PLANUL TĂU CURENT:
%s

SARcinĂ DE REFLECȚIE ȘI CORECTARE:
1. Analizează cu atenție planul tău. Dacă în cerință utilizatorul menționează explicit fișiere din care să "te folosești", să "te integrezi cu ele" sau să îți "îmbogățești cunoștințele" (ex: "folosindu-te de X", "citind din Y"), AI FOLOSIT câmpul "read_files"?
2. Dacă ai uitat să pui fișierele menționate în array-ul "read_files" la pașii relevanți (de tip "edit_file"), MODIFICĂ planul acum.
3. Exemplu: Dacă trebuie să editezi 'index.html' folosind 'script_shopping_cart.py', pasul tău trebuie să arate cam așa: 
   "action": "edit_file", "file_path": "index.html", "read_files": ["script_shopping_cart.py"].

Rescrie planul JSON final, corectat. Returnează DOAR JSON-ul.`, userInput, draftJSONStr)

	reqBody2 := ChatRequest{
		Model: plannerModel,
		Messages: []ChatMessage{
			// Păstrăm contextul: Îi dăm promptul inițial...
			{Role: "system", Content: prompt},
			// Apoi îi dăm sarcina de reflecție...
			{Role: "user", Content: reflectionPrompt},
		},
		Stream: false,
		Options: map[string]interface{}{
			"temperature": 0.1,
		},
	}

	jsonData2, _ := json.Marshal(reqBody2)
	resp2, err := http.Post(ollamaBaseURL+"/chat", "application/json", bytes.NewBuffer(jsonData2))
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()

	body2, _ := io.ReadAll(resp2.Body)
	var chatResp2 ChatResponse
	json.Unmarshal(body2, &chatResp2)

	finalJSONStr, found := extractJSONFromText(chatResp2.Message.Content)
	if !found {
		finalJSONStr = chatResp2.Message.Content
	}

	// =========================================================================
	// PASUL 3: PARSAREA FINALĂ
	// =========================================================================
	var plan ExecutionPlan
	if err := json.Unmarshal([]byte(finalJSONStr), &plan); err != nil {
		return nil, fmt.Errorf("Eroare la parsarea planului JSON corectat: %v. Răspuns brut: %s", err, finalJSONStr)
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

	hints.WriteString("🔍 [DIAGNOSTIC CRITIC PENTRU PLANIFICATOR]:\n")
	hints.WriteString(fmt.Sprintf("- Comanda care a eșuat: `%s`\n", failedCmd))

	// 1. Corelarea Comenzii cu Eroarea (Reasoning Logic)
	if strings.Contains(cmdLower, "python") && strings.Contains(errorLogLower, "indexerror") {
		hints.WriteString("- 💡 Diagnostic: Scriptul Python a încercat să acceseze un element dintr-o listă care nu există.\n")
		hints.WriteString("- 📂 Sursă Probabilă: Modul în care scriptul procesează (split) datele din fișierele text.\n")
		hints.WriteString("- ⚠️ RECOMANDARE: Verifică dacă fișierul citit are formatul așteptat (ex: conține caracterul ':' folosit la split).\n")
	}

	// 3. Detecție Pachete (doar dacă eroarea indică clar importuri)
	if strings.Contains(errorLogLower, "import") || strings.Contains(errorLogLower, "module") {
		hints.WriteString("- 📦 Lipsă dependințe: Se recomandă instalarea pachetului lipsă via terminal.\n")
	}

	hints.WriteString("\n🧠 CHAIN OF THOUGHT RECOMANDAT:\n")
	hints.WriteString("1. Citește fișierul implicat în comandă folosind 'read_files' pentru a vedea codul și datele de intrare.\n")
	hints.WriteString("2. Identifică neconcordanța între formatul datelor și logica de split.\n")
	hints.WriteString("3. Editează fișierul pentru a adăuga validări (ex: if len(parts) > 1).\n")

	return hints.String()
}
func validateAndFixPlan(plannerModel string, currentPlanJSON string, userInput string, projectTree string) (*ExecutionPlan, error) {
	fmt.Println("\n[Self-Critique] 🕵️ Master-ul verifică dacă planul are nevoie de date de test/fișiere inițiale...")

	validationPrompt := fmt.Sprintf(`Ești un inspector de calitate (QA Validator) sever pentru un sistem AI autonom.
Analizează planul JSON (Draft) de mai jos, conceput pentru a rezolva cerința utilizatorului: "%s"

ARBORELE FIȘIERELOR EXISTENTE PE DISC:
===========================
%s
===========================

PLANUL PROPUS (DRAFT):
%s

SARCINĂ CRITICĂ DE VALIDARE ȘI CORECTARE (APLICĂ ACESTE 4 REGULI):
1. VALIDARE DATE INIȚIALE (Anti-Crash): Dacă planul presupune citirea unui fișier, verifică dacă acel fișier EXISTĂ deja în Arborele de mai sus. Dacă NU există, scriptul va crăpa. CORECȚIE: ADAUGĂ OBLIGATORIU un nou pas la începutul planului (action: "edit_file") pentru a crea acel fișier cu câteva date de test (mock data) relevante.
2. VALIDARE CONTEXT (Enrichment): Dacă în cerință utilizatorul menționează explicit fișiere din care să citească, te-ai asigurat că folosești câmpul "read_files"? Dacă ai uitat, adaugă-l ("read_files": ["nume_fisier"]).
3. VALIDARE CERINȚĂ COMPLETĂ: Asigură-te că nu ai omis niciun detaliu sau pas de execuție ("execute_command") cerut inițial.
4. VALIDARE UNELTE STRICTE (Anti-Hallucination): Câmpul "action" din JSON POATE AVEA DOAR UNA DIN ACESTE 3 VALORI: "edit_file", "execute_command", "copy_code". ESTE STRICT INTERZIS să inventezi alte acțiuni (ex: "create_file", "run_script", "read_file" sunt invalide). CORECȚIE: Dacă găsești acțiuni inventate, înlocuiește-le cu cea corectă (ex: pentru a crea un fișier, folosește "edit_file").
5. VALIDARE INTEGRITATE IMPORTURI (Regulă Nouă): Analizează instrucțiunile de cod. Dacă un fișier (ex: main.py) depinde de altul (ex: database.py), asigură-te că planul include instrucțiuni EXPLICITE pentru Coder să adauge liniile de 'import' necesare. Dacă un pas de editare folosește funcții din alt fișier, acel fișier TREBUIE să fie în lista "read_files" a acelui pas pentru ca Coder-ul să îi vadă semnătura.

Rescrie planul JSON final, aplicând toate corecțiile de mai sus. Returnează DOAR codul JSON valid. FĂRĂ EXPLICAȚII SAU TEXT SUPLIMENTAR!`, userInput, projectTree, currentPlanJSON)

	reqBody := ChatRequest{
		Model: plannerModel,
		Messages: []ChatMessage{
			{Role: "user", Content: validationPrompt},
		},
		Stream: false,
		Options: map[string]interface{}{
			"temperature": 0.1, // Foarte mică pentru determinism
		},
	}

	jsonData, _ := json.Marshal(reqBody)
	resp, err := http.Post(ollamaBaseURL+"/chat", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var chatResp ChatResponse
	json.Unmarshal(body, &chatResp)

	finalJSONStr, found := extractJSONFromText(chatResp.Message.Content)
	if !found {
		finalJSONStr = chatResp.Message.Content
	}

	var plan ExecutionPlan
	if err := json.Unmarshal([]byte(finalJSONStr), &plan); err != nil {
		return nil, fmt.Errorf("Eroare la parsarea planului validat: %v", err)
	}

	return &plan, nil
}

// findPointAfterImports returnează indexul unde se termină secțiunea de importuri.
func findPointAfterImports(content string) int {
	// 1. Căutăm ultimul import (fie simplu, fie bloc)
	lastImportIdx := strings.LastIndex(content, "import")

	if lastImportIdx == -1 {
		// Dacă nu există importuri, căutăm după declarația 'package'
		pkgIdx := strings.Index(content, "package")
		if pkgIdx == -1 {
			return 0 // Fișier gol sau invalid
		}
		// Găsim sfârșitul liniei cu package
		lineEnd := strings.Index(content[pkgIdx:], "\n")
		if lineEnd == -1 {
			return len(content)
		}
		return pkgIdx + lineEnd + 1
	}

	// 2. Verificăm dacă importul este un bloc: import ( ... )
	rest := content[lastImportIdx:]
	// Verificăm dacă după "import" urmează o paranteză (ignorând spațiile)
	trimmedRest := strings.TrimSpace(rest[6:])
	if len(trimmedRest) > 0 && trimmedRest[0] == '(' {
		closeParen := strings.Index(rest, ")")
		if closeParen != -1 {
			return lastImportIdx + closeParen + 1
		}
	}

	// 3. Este un import pe o singură linie: import "fmt"
	lineEnd := strings.Index(rest, "\n")
	if lineEnd != -1 {
		return lastImportIdx + lineEnd + 1
	}

	return lastImportIdx + len("import")
}

// fuzzyReplace încearcă să înlocuiască textul inteligent, ignorând diferențele de spații/tab-uri
// fuzzyReplace încearcă să înlocuiască textul inteligent și aliniază automat indentarea (Python-safe)
func fuzzyReplace(content, search, replace string) (string, error) {
	words := strings.Fields(search)
	if len(words) == 0 {
		return content, nil
	}

	var escapedWords []string
	for _, w := range words {
		escapedWords = append(escapedWords, regexp.QuoteMeta(w))
	}

	pattern := strings.Join(escapedWords, `\s+`)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return content, fmt.Errorf("eroare regex: %v", err)
	}

	loc := re.FindStringIndex(content)
	if loc != nil {
		prefix := content[:loc[0]]
		suffix := content[loc[1]:]

		// --- MAGIA PENTRU ALINIEREA INDENTĂRII (Auto-Indent) ---
		lastNewline := strings.LastIndex(prefix, "\n")
		var originalIndent string
		if lastNewline != -1 {
			originalIndent = prefix[lastNewline+1:]
			// Verificăm dacă sunt doar spații sau taburi
			isSpace := true
			for _, c := range originalIndent {
				if c != ' ' && c != '\t' {
					isSpace = false
					break
				}
			}
			if isSpace {
				// Tăiem spațiile din prefix ca să nu le dublăm cu cele din replace
				prefix = prefix[:lastNewline+1]
			} else {
				originalIndent = ""
			}
		}

		replaceText := strings.Trim(replace, "\r\n") // Curățăm enter-urile
		lines := strings.Split(replaceText, "\n")

		if originalIndent != "" && len(lines) > 0 {
			// Aflăm indentarea pe care a halucinat-o AI-ul pe prima linie
			var aiIndent string
			for i, ch := range lines[0] {
				if ch != ' ' && ch != '\t' {
					aiIndent = lines[0][:i]
					break
				}
			}

			// Aplicăm corecția (shift) pe toate liniile generate de AI
			for i, line := range lines {
				if strings.HasPrefix(line, aiIndent) {
					// Înlocuim indentarea AI-ului cu indentarea originală a fișierului
					lines[i] = originalIndent + line[len(aiIndent):]
				} else if strings.TrimSpace(line) != "" {
					// Fallback pentru linii haotice: le aducem la nivelul de bază
					lines[i] = originalIndent + strings.TrimLeft(line, " \t")
				}
			}
			replaceText = strings.Join(lines, "\n")
		}

		return prefix + replaceText + suffix, nil
	}

	return content, fmt.Errorf("blocul de search nu a fost găsit (nici măcar fuzzy)")
}

// getSmartInsertionPoint găsește locul logic de inserare în funcție de tipul fișierului
func getSmartInsertionPoint(content string, filePath string) int {
	ext := strings.ToLower(filepath.Ext(filePath))

	switch ext {
	case ".go":
		return findPointAfterImports(content)

	case ".html":
		// În HTML, inserăm în interiorul ultimului bloc de script sau înainte de închiderea body
		if idx := strings.LastIndex(content, "</script>"); idx != -1 {
			return idx
		}
		if idx := strings.LastIndex(content, "</body>"); idx != -1 {
			return idx
		}
		return len(content)

	case ".py":
		// În Python, căutăm blocul principal 'if __name__ == "__main__":'
		// Vrem ca noile funcții să fie definite ÎNAINTE de execuția scriptului
		if idx := strings.Index(content, `if __name__ == "__main__":`); idx != -1 {
			return idx
		}
		// Dacă nu are bloc main, adăugăm la final
		return len(content)

	case ".css":
		// În CSS, noile reguli se adaugă întotdeauna la finalul fișierului
		// pentru a respecta cascada (să poată suprascrie stiluri anterioare)
		return len(content)

	default:
		// Pentru restul (.js, .json, .txt), finalul fișierului este cel mai sigur
		return len(content)
	}
}
func cleanXMLTags(text string) string {
	tagsToRemove := []string{
		"<diff>", "</diff>",
		"<search>", "</search>",
		"<replace>", "</replace>",
		"<search></search>",
	}

	for _, tag := range tagsToRemove {
		text = strings.ReplaceAll(text, tag, "")
	}

	// Tăiem DOAR enter-urile de la început și sfârșit.
	// PĂSTRĂM spațiile și tab-urile intacte pentru a nu strica Python-ul!
	text = strings.TrimLeft(text, "\r\n")
	text = strings.TrimRight(text, " \t\r\n")

	return text
}
func applyXMLDiff(originalContent string, llmResponse string, filePath string) (string, error) {
	// Găsim toate locațiile tag-urilor <search> și textul din interiorul lor
	searchRegex := regexp.MustCompile(`(?s)<search>(.*?)</search>`)

	// Folosim FindAllStringSubmatchIndex pentru a ști EXACT la ce index începe și se termină fiecare bucată
	searchMatches := searchRegex.FindAllStringSubmatchIndex(llmResponse, -1)

	if len(searchMatches) == 0 {
		return originalContent, fmt.Errorf("Nu am găsit niciun tag <search> valid")
	}

	modifiedContent := originalContent

	for i, matchLoc := range searchMatches {
		// matchLoc[2] și matchLoc[3] reprezintă începutul și sfârșitul textului din interiorul <search>
		searchText := strings.TrimSpace(llmResponse[matchLoc[2]:matchLoc[3]])

		// Calculăm zona unde ar trebui să fie codul de înlocuire pentru acest <search>
		replaceStart := matchLoc[1]    // Începe IMEDIAT după </search>
		replaceEnd := len(llmResponse) // Default: merge până la finalul mesajului generat de AI

		// Dacă mai există un alt tag <search> după acesta, ne oprim la el ca să nu îi furăm codul
		if i+1 < len(searchMatches) {
			replaceEnd = searchMatches[i+1][0]
		}

		// Aceasta este toată secțiunea brută de text care îi aparține acestui bloc
		rawReplaceBlock := llmResponse[replaceStart:replaceEnd]

		var replaceText string

		// 1. Încercăm să extragem <replace> clasic
		replRegex := regexp.MustCompile(`(?s)<replace>(.*?)</replace>`)
		replMatch := replRegex.FindStringSubmatch(rawReplaceBlock)

		if len(replMatch) > 0 {
			// AI-ul a fost cuminte și a pus <replace>
			replaceText = replMatch[1]
		} else {
			// 2. FALLBACK BLINDAT: A uitat <replace>.
			// Luăm ABSOLUT TOT ce a scris după </search>
			replaceText = rawReplaceBlock

			// Dacă din întâmplare a lăsat tag-ul de închidere </diff> la final, îl decupăm ca să rămână doar codul
			if idx := strings.LastIndex(replaceText, "</diff>"); idx != -1 {
				replaceText = replaceText[:idx]
			}
		}

		// 🔥 Trecem codul extras prin filtrul tău de curățare a tag-urilor
		replaceText = cleanXMLTags(replaceText)

		if searchText != "" {
			// CAZUL A: Modificare (Fuzzy Replace)
			newContent, err := fuzzyReplace(modifiedContent, searchText, replaceText)
			if err != nil {
				return modifiedContent, fmt.Errorf("Eroare la blocul #%d: %v", i+1, err)
			}
			modifiedContent = newContent
		} else {
			// CAZUL B: Inserție Smart
			insertionPoint := getSmartInsertionPoint(modifiedContent, filePath)

			prefix := modifiedContent[:insertionPoint]
			suffix := modifiedContent[insertionPoint:]

			// Inserăm codul cu formatare curată
			modifiedContent = prefix + "\n\n" + replaceText + "\n" + suffix
		}
	}

	return modifiedContent, nil
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

			// 1. Generarea planului inițial
			plan, err := generateExecutionPlan(cfg.PlannerModel, userInput, projectTree, persistentDocs)
			if err != nil {
				fmt.Printf("❌ Nu am putut genera planul: %v\n", err)
				continue
			}

			// Convertim draft-ul înapoi în JSON string pentru a-l trimite la validare
			//	draftPlanBytes, _ := json.Marshal(draftPlan)

			// 2. Trecerea prin filtrul de Self-Critique (Validare QA)
			//plan, err := validateAndFixPlan(cfg.PlannerModel, string(draftPlanBytes), userInput, projectTree)
			//if err != nil {
			//	fmt.Printf("⚠️ Eroare la validarea planului (folosesc draft-ul inițial): %v\n", err)
			//	plan = draftPlan // Fallback de siguranță la planul inițial dacă validarea dă eroare
			//}

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
			// ---> NOU: Inițializăm Jurnalul de Memorie al Planului
			var istoricPasi strings.Builder
			istoricPasi.WriteString("CE AI FĂCUT PÂNĂ ACUM ÎN ACEST PLAN:\n")

			// ---> REZUMAT PENTRU ISTORIC (Ce salvăm în autodev_history.json)
			var rezumatSistem strings.Builder
			rezumatSistem.WriteString("[Sistem: Rezultatul execuției planului a fost următorul]\n")

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
						// ... (Logica ta de Smart Path Resolver rămâne exact la fel) ...
						actualPath := readFile
						if _, err := os.Stat(actualPath); os.IsNotExist(err) {
							filepath.Walk(".", func(p string, info os.FileInfo, err error) error {
								if err == nil && !info.IsDir() && info.Name() == filepath.Base(readFile) {
									actualPath = p
								}
								return nil
							})
						}

						foundInMemory := false
						for fpath := range fisiereModificateContext {
							if filepath.Base(fpath) == filepath.Base(actualPath) {
								fmt.Printf("   📚 [Enrichment] '%s' a fost cerut, dar este DEJA pus pe masă din Memoria de Plan.\n", actualPath)
								foundInMemory = true
								break
							}
						}

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

				switch step.Action {
				case "copy_code":
					args["filePath"] = step.FilePath
					args["sourceFile"] = step.SourceFile
					args["instructions"] = step.Instructions
					toolResult = handleToolCall("toolCopyCode", args, cfg.CoderModel, contextBuilder.String())
				case "edit_file":
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

				// --- VERIFICĂRI STATUS PAS ---

				if strings.Contains(toolResult, "OPRIT") || strings.Contains(toolResult, "respins") {
					fmt.Println("\n⚠️ Execuția planului a fost întreruptă de utilizator.")
					rezumatSistem.WriteString(fmt.Sprintf("- Pas %d [%s]: ⚠️ Anulat/Respins de utilizator.\n", i+1, step.Action))
					eroareDetectata = true
					break // Folosim break pentru a opri forțat următorii pași dacă utilizatorul refuză
				}

				if strings.Contains(toolResult, "Eroare") || strings.Contains(toolResult, "error") || strings.Contains(toolResult, "Exception") || strings.Contains(toolResult, "Traceback") {
					fmt.Printf("\n⚠️ Eroare detectată la Pasul %d! Pregătesc auto-repararea...\n", i+1)
					rezumatSistem.WriteString(fmt.Sprintf("- Pas %d [%s]: ❌ Eșuat cu eroare.\n", i+1, step.Action))

					sugestii := analyzeErrorAndSuggest(toolResult, step.Instructions)

					autoPrompt = fmt.Sprintf(`A apărut o EROARE CRITICĂ la executarea ultimului pas:
=========================================
%s
=========================================

%s

SARCINĂ STRICTĂ DE AUTO-REPARARE:
1. CITEȘTE TRACEBACK-UL ȘI SUGESTIILE DE MAI SUS. 
2. LOGICA DE REPARARE:
   - DACĂ eroarea zice "ModuleNotFoundError" sau "No module named", primul tău pas TREBUIE să fie o acțiune "execute_command" care să ruleze "pip install <nume_pachet>" (sau echivalentul în Node/Go).
   - DACĂ eroarea e de sintaxă sau logică în cod, folosește "edit_file" pentru a repara fișierul.
3. TESTEAZĂ: Ultimul pas din noul tău plan trebuie să fie întotdeauna "execute_command" pentru a rula scriptul/aplicația din nou și a verifica dacă s-a reparat.

Returnează DOAR JSON-ul cu noul plan de acțiune!`, toolResult, sugestii)

					eroareDetectata = true
					break
				}

				// --- SUCCES: Actualizăm Jurnalul și Rezumatul ---
				target := step.FilePath
				if target == "" {
					target = "terminal"
				}
				rezumatSistem.WriteString(fmt.Sprintf("- Pas %d [%s] pe '%s': ✅ Executat cu succes.\n", i+1, step.Action, target))
				istoricPasi.WriteString(fmt.Sprintf("-> Am executat cu succes '%s' pe '%s'.\n", step.Action, target))

				if step.Action == "edit_file" {
					newContentBytes, err := os.ReadFile(step.FilePath)
					if err == nil {
						fisiereModificateContext[step.FilePath] = string(newContentBytes)
					}
				}
			}

			// =================================================================
			// SALVARE MEMORIE INDIFERENT DE STATUS (Succes, Oprit sau Eroare)
			// =================================================================
			conversationHistory = append(conversationHistory, ChatMessage{Role: "user", Content: userInput})
			conversationHistory = append(conversationHistory, ChatMessage{
				Role:    "assistant",
				Content: rezumatSistem.String(),
			})
			saveHistory(conversationHistory)
			fmt.Println("\n💾 Rezumatul execuției a fost salvat în memoria pe termen scurt.")

			if !eroareDetectata {
				fmt.Println("✅ Plan finalizat cu succes 100%.")
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
