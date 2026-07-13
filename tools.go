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

// --- DEFINIREA TOOL-URILOR DELEGATE (Planificatorul nu scrie cod, doar deleagă) ---

var autoDevTools = []Tool{
	{
		Type: "function",
		Function: ToolFunction{
			Name:        "toolOpenFile",
			Description: "Citește conținutul unui fișier din proiect.",
			Parameters: ToolParameters{Type: "object",
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
			Description: "Execută o comandă in terminal.",
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
	case "toolOpenFile":
		filePathRaw, ok := args["filePath"].(string)
		if !ok || strings.TrimSpace(filePathRaw) == "" {
			return "Eroare: Argumentul 'filePath' lipsește sau este invalid."
		}

		// 1. Obținem directorul rădăcină (root) curent de lucru
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Sprintf("Eroare de sistem la obținerea directorului curent: %v", err)
		}

		// 2. Obținem calea absolută și o curățăm (elimină automat ./ sau ../ redundante)
		absPath, err := filepath.Abs(filePathRaw)
		if err != nil {
			return fmt.Sprintf("Eroare la procesarea căii fișierului: %v", err)
		}

		// 3. SECURITATED 1: Verificăm dacă fișierul este STRICT în interiorul directorului curent
		relPath, err := filepath.Rel(cwd, absPath)
		if err != nil || strings.HasPrefix(relPath, "..") || relPath == "." {
			return "Eroare de securitate: Nu poți citi fișiere din afara directorului rădăcină al proiectului!"
		}

		// 4. SECURITATE 2: Verificăm dacă orice director sau fișier din cale începe cu ".autodev"
		pathComponents := strings.Split(relPath, string(filepath.Separator))
		for _, part := range pathComponents {
			if strings.HasPrefix(part, ".autodev") {
				return "Eroare de securitate: Accesul la fișierele sau directoarele care încep cu '.autodev' este strict interzis!"
			}
		}

		// 5. Verificăm suplimentar cu funcția ta existentă de protecție (dacă este cazul)
		if isFileProtected(relPath) || isFileProtected(absPath) {
			return "Eroare: Nu poți interacționa cu fișiere protejate de sistem!"
		}

		// 6. Citim conținutul fișierului
		contentBytes, err := os.ReadFile(absPath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Sprintf("Eroare: Fișierul '%s' nu există în proiect.", relPath)
			}
			return fmt.Sprintf("Eroare la citirea fișierului '%s': %v", relPath, err)
		}

		// Returnăm conținutul fișierului către model
		return string(contentBytes)
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

		resp, err := CallOllamaGenerate(GlobalConfig.BaseURL, coderModel, prompt)
		if err != nil {
			return fmt.Sprintf("Eroare la generare: %v", err)
		}
		cleanCode := extractCodeFromMarkdown(resp)

		// Salvăm direct
		if err := os.WriteFile(targetPath, []byte(cleanCode), 0644); err != nil {
			return fmt.Sprintf("Eroare la salvare: %v", err)
		}
		return fmt.Sprintf(" Codul a fost integrat cu succes în %s de către %s.", targetPath, coderModel)
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

		fmt.Printf("\n[Sub-Agent]  Apelez %s pentru a %s fișierul...\n", coderModel, strings.ToLower(actionName))

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
- REGULĂ CRITICĂ PENTRU <search>: Textul din interiorul tag-ului <search> TREBUIE să fie o copie EXACTĂ (1:1) a codului existent în fișier. ESTE STRICT INTERZIS să inventezi comentarii, să trunchiezi codul sau să modifici conținutul vechi în <search>. Dacă inventezi cod acolo, sistemul se va bloca!
- Pentru a ADĂUGA o metodă/funcție nouă într-o CLASĂ existentă: NU lăsa <search> gol! Pune ultima metodă din acea clasă în <search> și scrie-o din nou în <replace> urmată imediat de metoda ta nouă.
- Pentru a ADĂUGA funcții globale / independente: Lasă <search></search> complet gol, iar sistemul le va insera corect la final.
- Pentru a adăuga în interiorul funcției main(): NU lăsa search gol. Pune ultima linie din main() în <search> și rescrie finalul funcției în <replace>.

- REGULĂ SINTAXĂ ABSOLUTĂ: Structura XML de diff trebuie să fie PERFECTĂ. 
  Fiecare deschidere de <search> trebuie să aibă OBLIGATORIU un </search> dedicat. 
  Fiecare deschidere de <replace> trebuie să aibă OBLIGATORIU un </replace> dedicat.
  ESTE STRICT INTERZIS să încurci tag-urile XML (search/replace) cu tag-urile HTML din interiorul codului (cum ar fi <div> sau </div>)!

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

		resp, err := CallOllamaGenerate(GlobalConfig.BaseURL, coderModel, prompt)
		if err != nil {
			return fmt.Sprintf("Eroare la generarea codului cu %s: %v", coderModel, err)
		}
		fmt.Println("\n================= DEBUG: RAW CODER RESPONSE =================")
		fmt.Println(resp)
		fmt.Println("=============================================================")
		cleanCode := extractCodeFromMarkdown(resp)

		// Dacă fișierul e NOU, acceptăm pur și simplu codul (fără search/replace)
		// Dacă fișierul e NOU, acceptăm pur și simplu codul (fără search/replace)
		if !fileExists {
			// 1. Eliminăm COMPLET orice bloc <search>...</search>, inclusiv tot conținutul său pe mai multe linii
			searchBlockRegex := regexp.MustCompile(`(?s)<search>.*?</search>`)
			cleanCode = searchBlockRegex.ReplaceAllString(cleanCode, "")

			// 2. Siguranță suplimentară: Dacă AI-ul a pus noul cod într-un bloc <replace>, extragem DOAR acel conținut
			replRegex := regexp.MustCompile(`(?s)<replace>(.*?)</replace>`)
			if replMatch := replRegex.FindStringSubmatch(cleanCode); len(replMatch) > 0 {
				cleanCode = replMatch[1]
			}

			// 3. Curățăm restul tag-urilor XML rămase (ex: <diff>, <![CDATA[, etc.) și spațiile goale
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
				fmt.Printf(" Backup creat la: %s\n", backupPath)
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
	fmt.Printf("\n[ATENȚIE] AI-ul Planificator a cerut acțiunea:\n   => %s\n", actionDesc)
	fmt.Print("Aprobați execuția? [y/n] :")

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
