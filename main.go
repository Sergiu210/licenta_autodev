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
	Action       string   `json:"action"`               // "create_file", "edit_file", "execute_command"
	FilePath     string   `json:"file_path"`            // Calea fișierului țintă (dacă e cazul)
	Instructions string   `json:"instructions"`         // Ce trebuie să facă Qwen sau ce comandă să ruleze
	ReadFiles    []string `json:"read_files,omitempty"` // <-- NOU: Memoria de Pas (Context Enrichment)
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
		referenceContext, _ := args["referenceContext"].(string)
		if isFileProtected(path) {
			return "Eroare: Acces interzis. Nu poți modifica fișiere de sistem sau biblioteci externe!"
		}

		approved, feedback := askUserConfirmation(fmt.Sprintf("Modificare fișier: `%s`\n   Instrucțiuni reparare către Coder: %s", path, instructions))
		if !approved {
			return feedback
		}

		currentContentBytes, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("Eroare citire: %v", err)
		}
		currentContent := string(currentContentBytes)

		fmt.Printf("\n[Sub-Agent] 🧠 Apelez %s pentru a repara/modifica fișierul...\n", coderModel)
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
		// 1. PROMPT OPTIMIZAT ANTI-LENE
		prompt := fmt.Sprintf(`Ești un expert programator.
Trebuie să modifici următorul fișier conform cerinței.
%s
================= FIȘIER ȚINTĂ DE MODIFICAT (%s) =================
%s
==================================================================

CERINȚĂ MODIFICARE:
%s

REGULI ABSOLUTE:
1. Returnează codul COMPLET și EXACT DOAR pentru fișierul țintă (%s).
2. ESTE STRICT INTERZIS să omiți cod folosind "// restul codului aici".
3. NU scrie și NU returna cod pentru fișierele din secțiunea de Referință.
4. Răspunde doar cu codul încapsulat în blocuri markdown.
`, refBlock, path, currentContent, instructions, path)

		resp, err := CallOllamaGenerate(coderModel, prompt)
		if err != nil {
			return fmt.Sprintf("Eroare la modificarea codului cu %s: %v", coderModel, err)
		}

		cleanCode := extractCodeFromMarkdown(resp)

		// 2. VALIDARE ANTI-TRUNCHIERE (SAFETY CHECK)
		// Verificăm dacă AI-ul a lăsat comentarii de "lene"
		lazyKeywords := []string{"// ...", "/* ... */", "// restul codului", "// rest of"}
		for _, kw := range lazyKeywords {
			if strings.Contains(strings.ToLower(cleanCode), kw) {
				return "Eroare: AI-ul a trunchiat codul (a folosit comentarii în loc să scrie tot codul). Modificarea a fost anulată pentru siguranță."
			}
		}

		// Verificăm diferența de dimensiune (dacă fișierul nou e cu 80% mai mic, e suspect)
		if len(cleanCode) < int(float64(len(currentContent))*0.2) {
			return "Eroare: Codul generat este suspect de mic față de original. Pentru a preveni ștergerea fișierului, acțiunea a fost blocată."
		}

		// 3. SISTEM DE BACKUP AUTOMAT
		backupPath := path + ".bak"
		err = os.WriteFile(backupPath, currentContentBytes, 0644)
		if err != nil {
			fmt.Printf("⚠️ Nu s-a putut crea backup pentru %s: %v\n", path, err)
		} else {
			fmt.Printf("📦 Backup creat la: %s\n", backupPath)
		}

		// 4. SCRIEREA FINALĂ
		if err := os.WriteFile(path, []byte(cleanCode), 0644); err != nil {
			return fmt.Sprintf("Eroare la salvarea fișierului modificat: %v", err)
		}

		return fmt.Sprintf("Fișierul a fost rescris cu succes de %s. Un backup a fost salvat ca %s.", coderModel, backupPath)
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

REGULI ABSOLUTE DE PLANIFICARE (CITEȘTE CU ATENȚIE):
1. REGULA PENTRU RULARE (PRIORITATE MAXIMĂ): Dacă utilizatorul cere doar să "ruleze", "testeze" sau "execute" o aplicație, generează EXACT UN SINGUR PAS de tip "execute_command". ESTE STRICT INTERZIS să generezi "edit_file" dacă utilizatorul nu a cerut reparații sau modificări!
2. REGULA PENTRU EDITARE: Folosește acțiunea "edit_file" DOAR dacă utilizatorul cere explicit să modifici, să adaugi, să scrii sau să repari ceva într-un fișier.
3. CREARE: Folosește "create_file" DOAR dacă fișierul nu există. Când creezi, scrie în "instructions" absolut tot ce trebuie să conțină fișierul.
4. CONTEXT ADIȚIONAL (read_files): Când editezi și ai nevoie de informații dintr-un alt fișier, adaugă-l în lista "read_files". Asta înseamnă DOAR CITIRE.
5. MUTARE COD: DACĂ utilizatorul cere explicit să "MUȚI" (move) cod dintr-un fișier în altul, generează 2 pași: Pas 1 - "edit_file" pe destinație (cu sursa în read_files), Pas 2 - "edit_file" pe sursă (pentru a șterge codul).
6. CÂMPURI OBLIGATORII: Fiecare pas din JSON TREBUIE să conțină "action", "file_path" și "instructions".
EXEMPLU 1 - RULARE SIMPLĂ (fără editare):
{
  "steps": [
    {
      "action": "execute_command",
      "file_path": "",
      "instructions": "python main.py"
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

EXEMPLU 3 - Mutare cod:
{
  "steps": [
    {
      "action": "edit_file",
      "file_path": "destinatie.go",
      "instructions": "Copiaza functia X din fisierul sursa aici.",
      "read_files": ["sursa.go"]
    },
    {
      "action": "edit_file",
      "file_path": "sursa.go",
      "instructions": "Sterge functia X din acest fisier deoarece a fost mutata."
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
REGULI ABSOLUTE PENTRU TOOL-URI:
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
						// Evităm duplicatele: dacă fișierul e deja în Memoria de Plan (a fost modificat proaspăt), nu îl mai citim iar de pe disc
						if _, alreadyInPlan := fisiereModificateContext[readFile]; !alreadyInPlan {
							fileContent, err := os.ReadFile(readFile)
							if err == nil {
								contextReferinta.WriteString(fmt.Sprintf("\n[MEMORIA DE PAS - Sursă Citire: %s]\n```\n%s\n```\n", readFile, string(fileContent)))
								fmt.Printf("   📚 [Enrichment] Am atașat context din: %s\n", readFile)
							} else {
								fmt.Printf("   ⚠️ [Avertisment] Nu am putut citi fișierul de context: %s\n", readFile)
							}
						}
					}
				}

				args["referenceContext"] = contextReferinta.String()

				switch step.Action {
				case "create_directory":
					args["dirPath"] = step.FilePath
					toolResult = handleToolCall("toolCreateDirectory", args, cfg.CoderModel, contextBuilder.String())
				case "create_file":
					args["filePath"] = step.FilePath
					args["instructions"] = step.Instructions
					toolResult = handleToolCall("toolCreateFile", args, cfg.CoderModel, contextBuilder.String())
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

				if step.Action == "edit_file" || step.Action == "create_file" {
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
