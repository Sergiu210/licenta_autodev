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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ledongthuc/pdf"
)

const cacheFileName = ".autodev_cache.json"

type FileCache map[string]string

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

// Citire PDF
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
	_, err = buf.ReadFrom(b)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Citire DOCX
// Citire DOCX (100% Open Source, folosind librăriile standard din Go)
func readDocx(path string) (string, error) {
	// Deschidem fișierul docx ca pe o arhivă ZIP
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer r.Close()

	var textBuilder strings.Builder

	// Căutăm fișierul principal de text în interiorul arhivei
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
					break // Am ajuns la finalul fișierului
				}

				switch se := token.(type) {
				case xml.StartElement:
					// <w:t> reprezintă un element de text în formatul Word
					if se.Name.Local == "t" {
						var text string
						decoder.DecodeElement(&text, &se)
						textBuilder.WriteString(text)
					}
				case xml.EndElement:
					// <w:p> reprezintă finalul unui paragraf
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

// Funcție centralizată care rutează citirea în funcție de extensie
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
func toolCreateFile(filePath string, content string) string {
	fmt.Printf("\n[Sistem] AI-ul a cerut crearea fișierului: %s\n", filePath)

	// Opțional: Poți verifica dacă fișierul există deja pentru a evita suprascrierea accidentală
	if _, err := os.Stat(filePath); err == nil {
		return fmt.Sprintf("Eroare: Fișierul %s există deja. Modificarea este interzisă conform regulilor.", filePath)
	}

	// Asigurăm crearea directoarelor părinte dacă nu există (ex: folder1/folder2/fisier.go)
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Sprintf("Eroare la crearea directoarelor: %v", err)
	}

	// Creăm sau scriem în fișier
	err := os.WriteFile(filePath, []byte(content), 0644)
	if err != nil {
		return fmt.Sprintf("Eroare la scrierea fișierului: %v", err)
	}

	fmt.Printf("[Sistem] Fișier creat cu succes: %s\n", filePath)
	return "Fișierul a fost creat cu succes."
}

// --- TOOL 2: Execuție comandă cu Human-in-the-Loop ---
// LLM-ul va oferi o comandă (ex: "go mod tidy" sau "go test ./...")
func toolExecuteCommand(command string) string {
	fmt.Printf("\n⚠️  [ATENȚIE - Human-in-the-loop] AI-ul vrea să ruleze următoarea comandă:\n")
	fmt.Printf("   $ %s\n", command)
	fmt.Print("Aprobare execuție? [Y/n]: ")

	// Citim răspunsul utilizatorului
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))

		// Dacă scrii 'n' sau 'no', oprim comanda
		if answer == "n" || answer == "no" {
			fmt.Println("[Sistem] Comanda a fost respinsă de utilizator.")
			return "Utilizatorul a respins executarea comenzii."
		}
	}

	fmt.Println("[Sistem] Execut comanda...")

	// Configurăm execuția în funcție de sistemul de operare (tu folosești Windows)
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", command)
	} else {
		cmd = exec.Command("bash", "-c", command)
	}

	// Rulăm comanda și capturăm output-ul (inclusiv erorile)
	output, err := cmd.CombinedOutput()

	result := string(output)
	if err != nil {
		fmt.Printf("[Eroare Comandă]: %v\n", err)
		result += fmt.Sprintf("\nEroare la execuție: %v", err)
	}

	fmt.Printf("[Rezultat Comandă]:\n%s\n", result)

	// Returnăm rezultatul înapoi către LLM ca să "vadă" ce s-a întâmplat (ex: dacă testul a trecut sau nu)
	return result
}

// --- SIMULATOR LLM ---
// Această funcție imită comportamentul unui LLM care decide să folosească unelte.
// --- SIMULATOR LLM ---
func simulateLLMResponse(userInput string) {
	fmt.Println("\n🤖 [Mock LLM]: Încep ciclul de scriere și verificare (Self-Healing)...")

	// === PASUL 1: PLAN & ACT (Scriem codul inițial cu o EROARE) ===
	caleFisier := "proiect_generat/main.go"

	// AICI E CORECTAT: Lipsește intenționat ghilimeaua, dar avem acolada `}` de final!
	codCuEroare := `package main

import "fmt"

func main() {
	fmt.Println("Test efectuat cu succes)
}
`
	fmt.Println("\n---> [Mock LLM] Creez fișierul de bază...")
	toolCreateFile(caleFisier, codCuEroare)

	// === PASUL 1.5: INIȚIALIZARE MEDIU (go mod) ===
	fmt.Println("\n---> [Mock LLM] Inițializez modulul Go...")
	comandaInit := "cd proiect_generat && (go mod init proiect_generat || echo Modul deja initializat) && go mod tidy"
	toolExecuteCommand(comandaInit)

	// === PASUL 2: VERIFY ===
	comandaTestare := "cd proiect_generat && go run ."
	fmt.Println("\n---> [Mock LLM] Verific codul (VERIFY)...")
	rezultatComanda := toolExecuteCommand(comandaTestare)

	// === PASUL 3: DEBUG ===
	if strings.Contains(rezultatComanda, "Eroare la execuție") {

		fmt.Println("\n=======================================================")
		fmt.Println(" 🚨 ALARMĂ BACKEND: S-A DETECTAT O EROARE DE COMPILARE!")
		fmt.Println("=======================================================")
		fmt.Printf("🤖 [Mock LLM]: Eroare interceptată. Inițiez AUTO-CORECȚIA prin Diff...\n")

		// === PASUL 4: RE-ACT (Folosim TOOL DE EDITARE) ===

		// Folosim backticks pentru a căuta EXACT structura greșită, fără spații la final
		codVechi := `fmt.Println("Test efectuat cu succes)`

		// Înlocuim cu structura corectă
		codNou := `fmt.Println("Test efectuat cu succes") // <-- EROARE CORECTATĂ PRIN DIFF`

		fmt.Println("\n---> [Mock LLM] Apelează tool-ul: toolEditFile...")
		rezultatEditare := toolEditFile(caleFisier, codVechi, codNou)
		fmt.Printf("🤖 [Mock LLM] Raport editare: %s\n", rezultatEditare)

		// Re-verificăm codul
		fmt.Println("\n---> [Mock LLM] Re-verific codul modificat...")
		rezultatCorectat := toolExecuteCommand(comandaTestare)

		if !strings.Contains(rezultatCorectat, "Eroare la execuție") {
			fmt.Println("\n✅ 🤖 [Mock LLM]: Auto-corecția a reușit folosind patch-ul Diff!")
		} else {
			fmt.Println("\n❌ 🤖 [Mock LLM]: Auto-corecția a eșuat.")
		}

	} else {
		fmt.Println("\n✅ 🤖 [Mock LLM]: Codul a rulat perfect din prima!")
	}
}
func toolEditFile(filePath string, searchBlock string, replaceBlock string) string {
	fmt.Printf("\n[Sistem] AI-ul a cerut modificarea (diff) fișierului: %s\n", filePath)

	// 1. Citim fișierul curent
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Sprintf("Eroare: Nu am putut citi fișierul %s: %v", filePath, err)
	}
	fileText := string(content)

	// 2. Verificăm dacă codul vechi există EXACT așa în fișier
	if !strings.Contains(fileText, searchBlock) {
		return "Eroare Diff: Nu am găsit 'searchBlock' în fișier. Asigură-te că textul vechi se potrivește EXACT cu cel din fișier (inclusiv spațiile și tab-urile)."
	}

	// 3. Aplicăm modificarea (înlocuim doar prima apariție pentru siguranță)
	newText := strings.Replace(fileText, searchBlock, replaceBlock, 1)

	// 4. Scriem la loc pe disc
	err = os.WriteFile(filePath, []byte(newText), 0644)
	if err != nil {
		return fmt.Sprintf("Eroare la salvarea modificărilor: %v", err)
	}

	fmt.Printf("[Sistem] Modificarea a fost aplicată cu succes în: %s\n", filePath)
	return "Modificare aplicată cu succes."
}

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("Eroare la obținerea directorului curent: %v\n", err)
		return
	}

	// --- LOGICA DE SCANARE ȘI CACHING (PĂSTRATĂ) ---
	fmt.Printf("--- Analiză fișiere în: %s ---\n", cwd)
	cache := loadCache(cwd)
	newCache := make(FileCache)
	var contextBuilder strings.Builder
	fisiereProcesate := 0

	err = filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && (strings.HasPrefix(info.Name(), ".") || info.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if info.IsDir() || info.Name() == cacheFileName || info.Name() == "autodev.exe" || info.Name() == "autodev" {
			return nil
		}

		textContent, err := getContent(path)
		if err != nil {
			return nil
		}

		contentBytes := []byte(textContent)
		currentHash := computeHash(contentBytes)
		relPath, _ := filepath.Rel(cwd, path)
		newCache[relPath] = currentHash

		oldHash, exists := cache[relPath]
		if !exists || oldHash != currentHash {
			fmt.Printf("[MODIFICAT/NOU] %s\n", relPath)
			fisiereProcesate++
			contextBuilder.WriteString(fmt.Sprintf("\n--- Fișier: %s ---\n%s\n", relPath, textContent))
		}
		return nil
	})

	if err != nil {
		fmt.Printf("Eroare la scanare: %v\n", err)
	}
	saveCache(cwd, newCache)

	if fisiereProcesate > 0 {
		fmt.Printf("S-au indexat %d fișiere noi/modificate.\n", fisiereProcesate)
	} else {
		fmt.Println("Nicio modificare detectată în fișiere.")
	}

	// --- LOGICA DE INTERACȚIUNE CONTINUĂ (REPL) ---
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("\n====================================================")
	fmt.Println("AUTODEV ACTIV - Introdu cerința (sau 'exit' pentru ieșire)")
	fmt.Println("====================================================")

	for {
		fmt.Print("\n> Cerința ta: ")

		// Citim input-ul de la utilizator
		if !scanner.Scan() {
			break
		}
		userInput := scanner.Text()

		// Comandă de oprire a programului
		if strings.ToLower(userInput) == "exit" {
			fmt.Println("Închidere program...")
			break
		}

		if strings.TrimSpace(userInput) == "" {
			continue
		}

		// ASAMBLAREA PROMPTULUI FINAL
		// Combinăm Contextul (fișierele) + Instrucțiunea ta
		var finalPrompt strings.Builder
		finalPrompt.WriteString("CONTEXT COD SURSĂ:\n")
		finalPrompt.WriteString(contextBuilder.String())
		finalPrompt.WriteString("\n-------------------------\n")
		finalPrompt.WriteString("CERINȚĂ UTILIZATOR: " + userInput)

		// Simulare trimitere către AI
		fmt.Println("\n[Sistem] Prompt asamblat cu succes!")
		fmt.Println("Lungime totală prompt:", len(finalPrompt.String()), "caractere.")

		// Aici vei insera în pasul următor apelul către LLM
		fmt.Println("Aștept integrarea LLM pentru a procesa: " + userInput)
		// Simulare trimitere către AI
		fmt.Println("\n[Sistem] Prompt asamblat cu succes! Se trimite către modelul AI...")

		// APELĂM SIMULATORUL (Înlocuitor pentru conexiunea reală la API)
		simulateLLMResponse(userInput)
	}
}
