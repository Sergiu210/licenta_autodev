package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var GlobalConfig *Config

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
		modele, err := GetLocalModels(cfg.BaseURL)
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
	printVaultCoderBanner()
	fmt.Println("\n====================================================")
	fmt.Println(" VAULTCODER: ARHITECTURĂ MASTER-WORKER ACTIVATĂ ")
	fmt.Printf(" Provider: %s\n", strings.ToUpper(cfg.Provider))
	if cfg.Provider == "ollama" {
		fmt.Printf(" Planificator: %s\n", cfg.PlannerModel)
		fmt.Printf(" Executant: %s\n", cfg.CoderModel)
	} else {
		fmt.Printf(" Model Cloud: %s\n", cfg.PlannerModel)
	}
	fmt.Println("Tastați '/setari' pentru a modifica backend-ul.")
	fmt.Println("Tastați '/clear' pentru a sterge istoricul.")
	fmt.Println("====================================================")
	scanner := bufio.NewScanner(os.Stdin)

	// --- MODIFICAT: Încărcăm memoria salvată anterior ---
	conversationHistory := loadHistory()
	if len(conversationHistory) > 0 {
		fmt.Printf(" [Sistem] Am încărcat %d mesaje anterioare din memorie.\n", len(conversationHistory))
	}
	var autoPrompt string
	var persistentDocs string //  Aici vom stoca documentația de proiect

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
					fmt.Printf("[Detectat/Modificat]  %s\n", relPath)
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
			fmt.Println("\n [Auto-Recuperare] Trimit automat eroarea către AI...")
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
				fmt.Println(" Te rog să specifici calea către fișier. Exemplu: /getcontext docs/specificatii.md")
				continue
			}

			docPath := strings.TrimSpace(parts[1])
			content, err := getContent(docPath) // Folosim funcția ta magică care citește și PDF/Docx!

			if err != nil {
				fmt.Printf(" Eroare la citirea documentației '%s': %v\n", docPath, err)
			} else {
				persistentDocs = fmt.Sprintf("\n================= DOCUMENTAȚIE / SPECIFICAȚII PROIECT =================\n%s\n=======================================================================\n", content)
				fmt.Printf(" Documentația din '%s' a fost încărcată cu succes în memoria permanentă!\n", docPath)
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
			fmt.Println("\n [Sistem] Memoria conversației a fost ștearsă cu succes!")
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

				responseMsg, err := CallOllamaChat(cfg.BaseURL, cfg.PlannerModel, mesajeChat, autoDevTools)
				if err != nil {
					fmt.Println("\n [Eroare CHAT]:", err)
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

					finalResponse, err := CallOllamaChat(cfg.BaseURL, cfg.PlannerModel, mesajeChat, nil)
					if err == nil {
						fmt.Println("\n [Asistent]:", finalResponse.Content)
						conversationHistory = append(conversationHistory, ChatMessage{Role: "user", Content: userInput}, finalResponse)
						saveHistory(conversationHistory)
					}
				} else {
					fmt.Println("\n [Asistent]:", responseMsg.Content)
					conversationHistory = append(conversationHistory, ChatMessage{Role: "user", Content: userInput}, responseMsg)
					saveHistory(conversationHistory)
				}
				continue // Ne întoarcem la începutul buclei, am terminat cu CHAT-ul
			}

			// Dacă a ajuns aici, înseamnă că intent == "CODE"
			fmt.Printf("\n[Planificator]  `%s` analizează cerința și creează planul de acțiune...\n", cfg.PlannerModel)

			// 1. Generarea planului inițial
			plan, err := generateExecutionPlan(cfg.PlannerModel, userInput, projectTree, persistentDocs)
			if err != nil {
				fmt.Printf(" Nu am putut genera planul: %v\n", err)
				continue
			}

			if len(plan.Steps) == 0 {
				fmt.Println(" Planul generat este gol. Probabil cerința a fost neclară.")
				continue
			}

			fmt.Println("\n PLAN DE EXECUȚIE DETECTAT:")
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
				fmt.Printf("\n Execut Pasul %d: %s\n", i+1, step.Action)

				var toolResult string
				args := make(map[string]interface{})

				// --- PREGATIRE MEMORIE (PLAN + PAS) ---
				var contextReferinta strings.Builder

				// A. Memoria de Plan (Fisiere deja modificate în pașii anteriori)
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
								fmt.Printf("    [Enrichment] '%s' a fost cerut, dar este DEJA pus pe masă din Memoria de Plan.\n", actualPath)
								foundInMemory = true
								break
							}
						}

						if !foundInMemory {
							fileContent, err := os.ReadFile(actualPath)
							if err == nil {
								contextReferinta.WriteString(fmt.Sprintf("\n[MEMORIA DE PAS - Sursă Citire: %s]\n```\n%s\n```\n", actualPath, string(fileContent)))
								fmt.Printf("    [Enrichment] Am atașat context de pe DISC din: %s\n", actualPath)
							} else {
								fmt.Printf("    [Avertisment] Nu am putut găsi fișierul de context : %s\n", readFile)
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

				// --- VERIFICARI STATUS PAS ---

				if strings.Contains(toolResult, "OPRIT") || strings.Contains(toolResult, "respins") {
					fmt.Println("\n Execuția planului a fost întreruptă de utilizator.")
					rezumatSistem.WriteString(fmt.Sprintf("- Pas %d [%s]:  Anulat/Respins de utilizator.\n", i+1, step.Action))
					eroareDetectata = true
					break // Folosim break pentru a opri fortat urmatorii pasi dacă utilizatorul refuza
				}

				if strings.Contains(toolResult, "Eroare") || strings.Contains(toolResult, "error") || strings.Contains(toolResult, "Exception") || strings.Contains(toolResult, "Traceback") {
					fmt.Printf("\n Eroare detectată la Pasul %d! Pregătesc auto-repararea...\n", i+1)
					rezumatSistem.WriteString(fmt.Sprintf("- Pas %d [%s]:  Eșuat cu eroare.\n", i+1, step.Action))

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

REGULI ABSOLUTE PENTRU JSON (ANTI-CRASH):
- "action" POATE FI DOAR UNA DIN ACESTE 3: "edit_file", "execute_command", "copy_code". ESTE STRICT INTERZIS să inventezi alte acțiuni (NU folosi "read_files" ca acțiune).
- "file_path" TREBUIE SĂ FIE STRICT UN STRING (o singură cale), NICIODATĂ O LISTĂ [].
- Dacă ai nevoie să citești fișiere pentru context, adaugă proprietatea "read_files": ["fisier_necesar.py"] EXCLUSIV în interiorul unui pas "edit_file".

EXEMPLU DE PLAN JSON VALID DE REPARARE:
{
  "steps": [
    {
      "action": "edit_file",
      "file_path": "main.py",
      "read_files": ["config.py"],
      "instructions": "Adaugă verificarea if len(parts) > 1 înainte de a face split, pentru a preveni IndexError semnalat."
    },
    {
      "action": "execute_command",
      "file_path": "",
      "instructions": "python main.py"
    }
  ]
}

Returnează DOAR JSON-ul valid cu noul plan de acțiune! FĂRĂ EXPLICAȚII.`, toolResult, sugestii)

					eroareDetectata = true
					break
				}

				// --- SUCCES: Actualizam Jurnalul și Rezumatul ---
				target := step.FilePath
				if target == "" {
					target = "terminal"
				}
				rezumatSistem.WriteString(fmt.Sprintf("- Pas %d [%s] pe '%s':  Executat cu succes.\n", i+1, step.Action, target))
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
			fmt.Println("\n Rezumatul execuției a fost salvat în memoria pe termen scurt.")

			if !eroareDetectata {
				fmt.Println(" Plan finalizat cu succes 100%.")
			}

		} else {
			// =================================================================
			// RAMURA PENTRU MODELE CLOUD (OpenAI / Gemini / Anthropic)
			// =================================================================
			fmt.Printf("\n [Cloud Agent] Apelez %s (%s) pentru a rezolva cerința...\n", strings.ToUpper(cfg.Provider), cfg.PlannerModel)

			fmt.Println(" Logica de execuție pentru Cloud este în curs de implementare.")
		}
	}
}

// Banner-ul ASCII desenat pentru VaultCoder
func printVaultCoderBanner(color ...string) {
	// Culoarea implicită (Electric Purple)
	selectedColor := "\033[38;2;138;43;226m"
	const neonPink = "\033[38;2;255;20;147m"       // Roz electric
	const cyberBlue = "\033[38;2;0;255;255m"       // Albastru Cyan deschis
	const electricPurple = "\033[38;2;138;43;226m" // Mov intens
	const hackerMint = "\033[38;2;0;250;154m"      // Verde mentă / Matrix luminos
	const reset = "\033[0m"

	// --- STILURI DE TEXT ---
	const boldText = "\033[1m"
	// Dacă s-a transmis o culoare în apelul funcției, o folosim pe aceea
	if len(color) > 0 && color[0] != "" {
		selectedColor = color[0]
	}

	logo := "\n" +
		" __     __            _ _     ____          _           \n" +
		" \\ \\   / /_ _ _   _  | | |_  / ___|___   __| | ___ _ __ \n" +
		"  \\ \\ / / _` | | | | | | __|| |   / _ \\ / _` |/ _ \\ '__|\n" +
		"   \\ V / (_| | |_| | | | |_ | |__| (_) | (_| |  __/ |   \n" +
		"    \\_/ \\__,_|\\__,_|_|_|\\__| \\____\\___/ \\__,_|\\___|_|   \n"

	fmt.Print(boldText, selectedColor, logo, reset)
}
