package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

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
			"top_p":       0.5,
		},
	}

	jsonData, _ := json.Marshal(reqBody)
	resp, err := http.Post(GlobalConfig.BaseURL+"/chat", "application/json", bytes.NewBuffer(jsonData))
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

3. DETALIEREA INSTRUCȚIUNILOR (CRITIC): Câmpul "instructions" trebuie să fie EXHAUSTIV. Este STRICT INTERZIS să fii vag (ex: "importă librării" sau "fă scriptul"). Trebuie să incluzi absolut toate detaliile, constrângerile, timpii, numele variabilelor și logica dorită de utilizator. Coder-ul nu vede cerința inițială, scrie codul STRICT pe baza a ce îi pui tu în "instructions"!

EXEMPLU EDITARE CU ENRICHMENT (CUM CITEȘTI CONTEXTUL):
{
  "steps": [
    {
      "action": "edit_file",
      "file_path": "frontend.js",
      "instructions": "Extrage endpoint-urile din server.py. Implementează un fetch către '/api/data', gestionează erorile de rețea cu un block try/catch și randează rezultatul în DOM în elementul cu id-ul 'container'.",
      "read_files": ["server.py"] 
    }
  ]
}

EXEMPLU PENTRU SCRIPTURI:
{
  "steps": [
    {
      "action": "edit_file",
      "file_path": "terminal_game.py",
      "instructions": "Implementează logica jocului. 1. Folosește librăria curses sau rich. 2. Desenează o bilă care ricoșează de marginile terminalului. 3. Setează un game loop care face update la ecran la fiecare 100ms. 4. Oprește execuția automat după exact 10 secunde."
    },
    {
      "action": "execute_command",
      "file_path": "",
      "instructions": "python terminal_game.py"
    }
  ]
}

REGULĂ VITALĂ: Exemplele de mai sus sunt DOAR DEMONSTRATIVE. Este STRICT INTERZIS să copiezi numele fișierelor din exemple. Creează un plan ORIGINAL.

Returnează STRICT cod JSON valid. FĂRĂ comentarii.

`, projectTree, persistentDocs, userInput)
	//Do not include comments, explanations, or trailing commas.
	// Cererea 1: Către Ollama
	reqBody1 := ChatRequest{
		Model: plannerModel,
		Messages: []ChatMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
		Options: map[string]interface{}{
			"temperature": 0.1,
			"top_p":       0.5,
		},
	}

	jsonData1, _ := json.Marshal(reqBody1)
	resp1, err := http.Post(GlobalConfig.BaseURL+"/chat", "application/json", bytes.NewBuffer(jsonData1))
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

	fmt.Printf("\n[Reflecție Internă]  Am generat planul draft. Verific dacă folosește enrichment (read_files)...\n")

	// =========================================================================
	// PASUL 2: REFLECȚIA ȘI CORECTAREA (SELF-CORRECTION)
	// =========================================================================
	reflectionPrompt := fmt.Sprintf(`Acesta este planul JSON pe care l-ai generat pentru a rezolva cerința: "%s"

PLANUL TĂU CURENT:
%s

SARCINĂ DE REFLECȚIE ȘI CORECTARE:
1. Analizează cu atenție planul tău. Dacă în cerință utilizatorul menționează explicit fișiere din care să "te folosești", să "te integrezi cu ele" sau să îți "îmbogățești cunoștințele" (ex: "folosindu-te de X", "citind din Y"), AI FOLOSIT câmpul "read_files"?
2. Dacă ai uitat să pui fișierele menționate în array-ul "read_files" la pașii relevanți (de tip "edit_file"), MODIFICĂ planul acum.
3. Exemplu: Dacă trebuie să editezi 'frontend.html' folosind 'exemplu.py', pasul tău trebuie să arate cam așa: 
   "action": "edit_file", "file_path": "frontend.html", "read_files": ["exemplu.py"].

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
			"top_p":       0.5,
		},
	}

	jsonData2, _ := json.Marshal(reqBody2)
	resp2, err := http.Post(GlobalConfig.BaseURL+"/chat", "application/json", bytes.NewBuffer(jsonData2))
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
func validateAndFixPlan(plannerModel string, currentPlanJSON string, userInput string, projectTree string) (*ExecutionPlan, error) {
	fmt.Println("\n[Self-Critique]  Master-ul verifică dacă planul are nevoie de date de test/fișiere inițiale...")

	validationPrompt := fmt.Sprintf(`Ești un inspector de calitate (QA Validator) pentru un sistem AI autonom.
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
			"temperature": 0.1,
			"top_p":       0.5,
		},
	}

	jsonData, _ := json.Marshal(reqBody)
	resp, err := http.Post(GlobalConfig.BaseURL+"/chat", "application/json", bytes.NewBuffer(jsonData))
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

// analyzeErrorAndSuggest scanează logul de eroare și generează sugestii pentru AI
func analyzeErrorAndSuggest(errorLog string, failedCmd string) string {
	var hints strings.Builder
	errorLogLower := strings.ToLower(errorLog)
	cmdLower := strings.ToLower(failedCmd)

	hints.WriteString(" [DIAGNOSTIC CRITIC PENTRU PLANIFICATOR]:\n")
	hints.WriteString(fmt.Sprintf("- Comanda care a eșuat: `%s`\n", failedCmd))

	// 1. Corelarea Comenzii cu Eroarea (Reasoning Logic)
	if strings.Contains(cmdLower, "python") && strings.Contains(errorLogLower, "indexerror") {
		hints.WriteString("-  Diagnostic: Scriptul Python a încercat să acceseze un element dintr-o listă care nu există.\n")
		hints.WriteString("-  Sursă Probabilă: Modul în care scriptul procesează (split) datele din fișierele text.\n")
		hints.WriteString("-  RECOMANDARE: Verifică dacă fișierul citit are formatul așteptat (ex: conține caracterul ':' folosit la split).\n")
	}

	// 3. Detecție Pachete (doar dacă eroarea indică clar importuri)
	if strings.Contains(errorLogLower, "import") || strings.Contains(errorLogLower, "module") {
		hints.WriteString("-  Lipsă dependențe: Se recomandă instalarea pachetului lipsă via terminal.\n")
	}

	hints.WriteString("\nCHAIN OF THOUGHT RECOMANDAT:\n")
	hints.WriteString("1. Citește fișierul implicat în comandă folosind 'read_files' pentru a vedea codul și datele de intrare.\n")
	hints.WriteString("2. Identifică neconcordanța între formatul datelor și logica de split.\n")
	hints.WriteString("3. Editează fișierul pentru a adăuga validări (ex: if len(parts) > 1).\n")

	return hints.String()
}
