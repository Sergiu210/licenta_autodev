package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const configFileName = "autodev_config.json"

type Config struct {
	Provider     string `json:"provider"`
	PlannerModel string `json:"planner_model"`
	CoderModel   string `json:"coder_model"`
	APIKey       string `json:"api_key"`
	BaseURL      string `json:"base_url"`
	EnableTools  bool   `json:"enable_tools"`
}

// Încarcă setările sau returnează un default sigur
func loadConfig() *Config {
	// 1. Definim valorile implicite (Default)
	cfg := &Config{
		Provider:    "ollama",
		BaseURL:     "http://localhost:11434/api",
		EnableTools: false, // <--- NOU: Activăm uneltele by default
	}

	// 2. Încercăm să citim fișierul de pe disc
	data, err := os.ReadFile(configFileName)
	if err == nil {
		// Dacă fișierul există, unmarshal va suprascrie valorile implicite
		// cu cele salvate de tine anterior
		json.Unmarshal(data, cfg)
	}

	// 3. Verificare de siguranță: dacă fișierul exista, dar BaseURL era gol, îl resetăm
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:11434/api"
	}
	GlobalConfig = cfg
	return cfg
}

func saveConfig(cfg *Config) {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(configFileName, data, 0644)
}

// --- Funcții de Caching și Citire ---
func showSettingsMenu(cfg *Config) {
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Println("\n=== ⚙️  MENIU SETĂRI VAULTCODER ===")
		fmt.Printf("1. Backend curent: %s\n", cfg.Provider)
		fmt.Printf("2. Model Planificator (Orchestrator): %s\n", cfg.PlannerModel)
		fmt.Printf("3. Model Coder (Lucrător): %s\n", cfg.CoderModel)
		fmt.Printf("4. API Key (Cloud): %s\n", func() string {
			if cfg.APIKey != "" {
				return "*** SETAT ***"
			}
			return "NESETAT"
		}())
		fmt.Printf("5. URL Server Ollama/vLLM: %s\n", cfg.BaseURL)
		stareTools := "DEZACTIVAT "
		if cfg.EnableTools {
			stareTools = "ACTIVAT "
		}
		fmt.Printf("6. Suport Unelte (Tools): %s\n", stareTools)
		fmt.Println("7. Ieșire și Salvare")
		fmt.Print("\nCe dorești să modifici? (1-7): ")
		if !scanner.Scan() {
			return
		}
		choice := strings.TrimSpace(scanner.Text())

		switch choice {
		case "1":
			providers := []string{"ollama", "vllm", "openai", "gemini"}
			fmt.Println("\n Alege noul backend:")
			for i, p := range providers {
				fmt.Printf("  %d. %s\n", i+1, p)
			}
			fmt.Print("\n Introdu numărul (1-4) sau scrie numele: ")

			if scanner.Scan() {
				input := strings.TrimSpace(scanner.Text())
				if input == "" {
					continue
				}

				// Verificăm dacă a introdus o cifră validă
				if idx, err := strconv.Atoi(input); err == nil && idx > 0 && idx <= len(providers) {
					cfg.Provider = providers[idx-1]
					fmt.Printf(" Backend setat la: %s\n", cfg.Provider)
				} else {
					// Verificăm dacă a scris direct numele provider-ului în loc de cifră
					inputLower := strings.ToLower(input)
					valid := false
					for _, p := range providers {
						if inputLower == p {
							cfg.Provider = p
							valid = true
							fmt.Printf(" Backend setat manual la: %s\n", cfg.Provider)
							break
						}
					}
					if !valid {
						fmt.Println(" Opțiune invalidă. Backend-ul a rămas neschimbat.")
					}
				}
			}
		case "2", "3":
			// Determinăm ce variabilă modificăm în funcție de alegere
			var target *string
			var roleName string
			if choice == "2" {
				target = &cfg.PlannerModel
				roleName = "Planificator (Orchestrator)"
			} else {
				target = &cfg.CoderModel
				roleName = "Coder (Lucrător)"
			}

			if cfg.Provider == "ollama" {
				// Dacă e Ollama, aducem modelele local
				modele, err := GetLocalModels(cfg.BaseURL)
				if err == nil && len(modele) > 0 {
					fmt.Printf("\n Modele Ollama instalate pentru %s:\n", roleName)
					for i, m := range modele {
						fmt.Printf("  %d. %s\n", i+1, m)
					}
					fmt.Print("\n Introdu numărul modelului (sau scrie manual numele): ")

					if scanner.Scan() {
						input := strings.TrimSpace(scanner.Text())
						if input == "" {
							continue
						}

						//  convertim input-ul într-un număr (index)
						if idx, err := strconv.Atoi(input); err == nil && idx > 0 && idx <= len(modele) {
							*target = modele[idx-1] // Setăm modelul ales prin număr
							fmt.Printf(" Model %s setat la: %s\n", roleName, *target)
						} else {
							// Dacă nu e număr, presupunem că a scris manual numele modelului
							*target = input
							fmt.Printf(" Model %s setat manual la: %s\n", roleName, *target)
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
					fmt.Println(" API Key actualizat!")
				}
			}
		case "5":
			fmt.Printf("Introdu noul URL (curent: %s)\n", cfg.BaseURL)
			fmt.Print("Exemplu: http://192.168.1.100:11434/api\n> ")
			if scanner.Scan() {
				input := strings.TrimSpace(scanner.Text())
				if input != "" {
					//  adăuga un sufix automat /api dacă utilizatorul uită
					if !strings.HasSuffix(input, "/api") {
						input = strings.TrimRight(input, "/") + "/api"
					}
					cfg.BaseURL = input
					fmt.Printf(" URL actualizat la: %s\n", cfg.BaseURL)
				}
			}
		case "6":
			cfg.EnableTools = !cfg.EnableTools
			if cfg.EnableTools {
				fmt.Println(" Uneltele (Tools) au fost ACTIVATE.")
			} else {
				fmt.Println("  Uneltele (Tools) au fost DEZACTIVATE.")
			}

		case "7": // Fostul case "6" devine "7"
			saveConfig(cfg)
			fmt.Println("Setări salvate cu succes!")
			return
		default:
			fmt.Println("Opțiune invalidă. Te rog alege o cifră de la 1 la 7.")
		}
	}
}
