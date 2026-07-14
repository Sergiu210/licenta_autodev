package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// --- DEFINIREA TOOL-URILOR DELEGATE (Planificatorul nu scrie cod, doar deleaga) ---

var autoDevTools = []Tool{
	{
		Type: "function",
		Function: ToolFunction{
			Name:        "toolOpenFile",
			Description: "Citeste continutul unui fisier din proiect.",
			Parameters: ToolParameters{Type: "object",
				Properties: map[string]ToolProperty{
					"filePath": {
						Type:        "string",
						Description: "Calea catre fisierul care trebuie citit,foloseste calea relativa",
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
			Description: "Executa o comanda in terminal.",
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
			Name:        "toolEditFile",
			Description: "Deleaga scrierea, crearea sau rescrierea unui fisier catre AI-ul Coder Expert. Daca fisierul nu exista, sistemul il va crea automat.",
			Parameters: ToolParameters{
				Type: "object",
				Properties: map[string]ToolProperty{
					"filePath":     {Type: "string", Description: "Calea catre fisierul de modificat sau creat."},
					"instructions": {Type: "string", Description: "Explica ce cod trebuie scris de la zero sau ce modificare logica trebuie aplicata."},
				},
				Required: []string{"filePath", "instructions"},
			},
		},
	},
}

// --- IMPLEMENTARE TOOL-URI (LOGICA MASTER-WORKER) ---
func handleToolCall(toolName string, args map[string]interface{}, coderModel string, projectContext string) string {
	switch toolName {
	case "toolOpenFile":
		filePathRaw, ok := args["filePath"].(string)
		if !ok || strings.TrimSpace(filePathRaw) == "" {
			return "Eroare: Argumentul 'filePath' lipseste sau este invalid."
		}

		// 1. Obtinem directorul radacina (root) curent de lucru
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Sprintf("Eroare de sistem la obtinerea directorului curent: %v", err)
		}

		// 2. Obtinem calea absoluta si o curatam (elimina automat ./ sau ../ redundante)
		absPath, err := filepath.Abs(filePathRaw)
		if err != nil {
			return fmt.Sprintf("Eroare la procesarea caii fisierului: %v", err)
		}

		// 3. SECURITATE 1: Verificam daca fisierul este STRICT in interiorul directorului curent
		relPath, err := filepath.Rel(cwd, absPath)
		if err != nil || strings.HasPrefix(relPath, "..") || relPath == "." {
			return "Eroare de securitate: Nu poti citi fisiere din afara directorului radacina al proiectului!"
		}

		// 4. SECURITATE 2: Verificam daca orice director sau fisier din cale incepe cu ".autodev"
		pathComponents := strings.Split(relPath, string(filepath.Separator))
		for _, part := range pathComponents {
			if strings.HasPrefix(part, ".autodev") {
				return "Eroare de securitate: Accesul la fisierele sau directoarele care incep cu '.autodev' este strict interzis!"
			}
		}

		// 5. Verificam suplimentar cu functia ta existenta de protectie (daca este cazul)
		if isFileProtected(relPath) || isFileProtected(absPath) {
			return "Eroare: Nu poti interactiona cu fisiere protejate de sistem!"
		}

		// 6. Citim continutul fisierului
		contentBytes, err := os.ReadFile(absPath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Sprintf("Eroare: Fisierul '%s' nu exista in proiect.", relPath)
			}
			return fmt.Sprintf("Eroare la citirea fisierului '%s': %v", relPath, err)
		}

		// Returnam continutul fisierului catre model
		return string(contentBytes)
	case "toolExecuteCommand":
		cmdRaw, ok := args["command"].(string)
		if !ok {
			return "Eroare: Argumentul 'command' lipseste."
		}
		if isCommandForbidden(cmdRaw) {
			return "Eroare: Comanda este interzisa."
		}

		approved, feedback := askUserConfirmation(fmt.Sprintf("Executa comanda: `%s`", cmdRaw))
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
			return "Eroare: Nu poti interactiona cu fisiere protejate!"
		}

		// Citim fisierul sursa
		sourceContentBytes, err := os.ReadFile(sourcePath)
		if err != nil {
			return fmt.Sprintf("Eroare: Sursa '%s' nu exista sau nu poate fi citita.", sourcePath)
		}

		// Citim / Cream destinatia
		targetExists := true
		targetContentBytes, err := os.ReadFile(targetPath)
		if err != nil {
			targetExists = false
			os.MkdirAll(filepath.Dir(targetPath), 0755)
		}
		targetContent := string(targetContentBytes)
		if !targetExists {
			targetContent = "// Fisier nou. Adauga codul copiat aici."
		}

		approved, feedback := askUserConfirmation(fmt.Sprintf("Copiere/Mutare din `%s` in `%s`\n   Instructiuni: %s", sourcePath, targetPath, instructions))
		if !approved {
			return feedback
		}

		prompt := fmt.Sprintf(`Esti un expert in integrarea codului.
Ai o memorie a pasilor:
%s

Ai misiunea de a TRANSFERA cod conform instructiunilor.
================= SURSA (%s) (De aici extragi) =================
%s
==================================================================
================= DESTINATIE (%s) (Aici inserezi/integrezi) ====
%s
==================================================================

INSTRUCTIUNI DE TRANSFER:
%s

REGULI ABSOLUTE:
1. Returneaza DOAR codul final COMPLET pentru fisierul DESTINATIE (%s).
2. Nu trunchia codul. Scrie-l complet si corect.
`, stepHistory, sourcePath, string(sourceContentBytes), targetPath, targetContent, instructions, targetPath)

		resp, err := CallOllamaGenerate(GlobalConfig.BaseURL, coderModel, prompt)
		if err != nil {
			return fmt.Sprintf("Eroare la generare: %v", err)
		}
		cleanCode := extractCodeFromMarkdown(resp)

		// Salvam direct
		if err := os.WriteFile(targetPath, []byte(cleanCode), 0644); err != nil {
			return fmt.Sprintf("Eroare la salvare: %v", err)
		}
		return fmt.Sprintf(" Codul a fost integrat cu succes in %s de catre %s.", targetPath, coderModel)
	case "toolEditFile":
		path, _ := args["filePath"].(string)
		instructions, _ := args["instructions"].(string)
		referenceContext, _ := args["referenceContext"].(string)
		stepHistory, _ := args["stepHistory"].(string) // Extragem Memoria!
		if isFileProtected(path) {
			return "Eroare: Acces interzis. Nu poti modifica fisiere de sistem sau biblioteci externe!"
		}

		// 1. Verificam daca fisierul exista
		fileExists := true
		currentContentBytes, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				fileExists = false
			} else {
				return fmt.Sprintf("Eroare la citirea fisierului: %v", err)
			}
		}

		actionName := "Modificare"
		if !fileExists {
			actionName = "Creare"
		}

		approved, feedback := askUserConfirmation(fmt.Sprintf("%s fisier: `%s`\n   Instructiuni: %s", actionName, path, instructions))
		if !approved {
			return feedback
		}

		currentContent := string(currentContentBytes)
		if !fileExists {
			currentContent = "// Acest fisier este nou si momentan este gol. Scrie codul de la zero conform cerintei."
		}

		fmt.Printf("\n[Sub-Agent]  Apelez %s pentru a %s fisierul...\n", coderModel, strings.ToLower(actionName))

		refBlock := ""
		if referenceContext != "" {
			refBlock = fmt.Sprintf(`
================= CONTEXT REFERINTA (NU MODIFICA) =================
Aceste fisiere au fost deja modificate la pasii anteriori. 
Priveste-le DOAR pentru a sti ce clase, ID-uri sau functii sa folosesti.
ESTE INTERZIS SA RETURNEZI COD PENTRU ACESTE FISIERE!
%s
===================================================================
`, referenceContext)
		}

		prompt := fmt.Sprintf(`Esti un expert programator.
Ai o memorie a pasilor executati anterior pentru a intelege contextul:
%s

%s
Ai primit sarcina sa modifici ACEST fisier:
<file_to_edit path="%s">
%s
</file_to_edit>

CERINTA:
%s

REGULI DE EDITARE:
- Pentru a MODIFICA: Pune codul vechi in <search> si cel nou in <replace>.
- REGULA CRITICA PENTRU <search>: Textul din interiorul tag-ului <search> TREBUIE sa fie o copie EXACTA (1:1) a codului existent in fisier. ESTE STRICT INTERZIS sa inventezi comentarii, sa trunchiezi codul sau sa modifici continutul vechi in <search>. Daca inventezi cod acolo, sistemul se va bloca!
- Pentru a ADAUGA o metoda/functie noua intr-o CLASA existenta: NU lasa <search> gol! Pune ultima metoda din acea clasa in <search> si scrie-o din nou in <replace> urmata imediat de metoda ta noua.
- Pentru a ADAUGA functii globale / independente: Lasa <search></search> complet gol, iar sistemul le va insera corect la final.
- Pentru a adauga in interiorul functiei main(): NU lasa search gol. Pune ultima linie din main() in <search> si rescrie finalul functiei in <replace>.

- REGULA SINTAXA ABSOLUTA: Structura XML de diff trebuie sa fie PERFECTA. 
  Fiecare deschidere de <search> trebuie sa aiba OBLIGATORIU un </search> dedicat. 
  Fiecare deschidere de <replace> trebuie sa aiba OBLIGATORIU un </replace> dedicat.
  ESTE STRICT INTERZIS sa incurci tag-urile XML (search/replace) cu tag-urile HTML din interiorul codului (cum ar fi <div> sau </div>)!

  <diff>
  <search>
  codul vechi care trebuie inlocuit
  </search>
  <replace>
  noul cod care vine in locul celui vechi
  </replace>
</diff>

Poti folosi mai multe blocuri <diff> daca trebuie sa modifici in mai multe locuri. Raspunde DOAR cu XML-ul, fara explicatii suplimentare.
`, stepHistory, refBlock, path, currentContent, instructions)

		resp, err := CallOllamaGenerate(GlobalConfig.BaseURL, coderModel, prompt)
		if err != nil {
			return fmt.Sprintf("Eroare la generarea codului cu %s: %v", coderModel, err)
		}
		fmt.Println("\n================= DEBUG: RAW CODER RESPONSE =================")
		fmt.Println(resp)
		fmt.Println("=============================================================")
		cleanCode := extractCodeFromMarkdown(resp)

		// Daca fisierul e NOU, acceptam pur si simplu codul (fara search/replace)
		if !fileExists {
			// 1. Eliminam COMPLET orice bloc <search>...</search>, inclusiv tot continutul sau pe mai multe linii
			searchBlockRegex := regexp.MustCompile(`(?s)<search>.*?</search>`)
			cleanCode = searchBlockRegex.ReplaceAllString(cleanCode, "")

			// 2. Siguranta suplimentara: Daca AI-ul a pus noul cod intr-un bloc <replace>, extragem DOAR acel continut
			replRegex := regexp.MustCompile(`(?s)<replace>(.*?)</replace>`)
			if replMatch := replRegex.FindStringSubmatch(cleanCode); len(replMatch) > 0 {
				cleanCode = replMatch[1]
			}

			// 3. Curatam restul tag-urilor XML ramase (ex: <diff>, <![CDATA[, etc.) si spatiile goale
			cleanCode = cleanXMLTags(cleanCode)
		} else {
			// Daca fisierul EXISTA, aplicam logica de DIFF XML
			newContent, errDiff := applyXMLDiff(currentContent, resp, path) // <--- Adauga path aici
			if errDiff != nil {
				return fmt.Sprintf("Eroare la aplicarea modificarilor (diff a esuat): %v\nRaspuns brut: %s", errDiff, resp)
			}
			cleanCode = newContent

			// SISTEM DE BACKUP AUTOMAT (pastram ce aveai deja)
			backupPath := path + ".bak"
			err = os.WriteFile(backupPath, currentContentBytes, 0644)
			if err == nil {
				fmt.Printf(" Backup creat la: %s\n", backupPath)
			}
		}

		// Cream folderele necesare
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Sprintf("Eroare la crearea directoarelor pentru %s: %v", path, err)
		}

		// SCRIEREA FINALA
		if err := os.WriteFile(path, []byte(cleanCode), 0644); err != nil {
			return fmt.Sprintf("Eroare la salvarea fisierului: %v", err)
		}

		statusMsg := "rescris"
		if !fileExists {
			statusMsg = "creat"
		}
		return fmt.Sprintf("Fisierul a fost %s cu succes de %s.", statusMsg, coderModel)
	default:
		return fmt.Sprintf("Eroare: Tool-ul %s nu este recunoscut.", toolName)
	}
}

// --- SIGURANTA SI VALIDARE ---

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
	fmt.Printf("\n[ATENTIE] AI-ul Planificator a cerut actiunea:\n   => %s\n", actionDesc)
	fmt.Print("Aprobati executia? [y/n] :")

	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		answer := strings.TrimSpace(scanner.Text())
		lowerAns := strings.ToLower(answer)
		if lowerAns == "y" || lowerAns == "yes" {
			return true, ""
		} else if lowerAns == "n" || lowerAns == "no" || lowerAns == "" {
			return false, "Utilizatorul a respins executia."
		} else {
			return false, fmt.Sprintf("Utilizatorul a OPRIT executia. Feedback: %s", answer)
		}
	}
	return false, "Eroare la citirea deciziei."
}
