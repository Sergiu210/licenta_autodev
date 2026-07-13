package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// applyXMLDiff primeste:
// - continutul original al fisierului,
// - raspunsul generat de modelul LLM,
// - calea fisierului.
//
// Functia interpreteaza raspunsul XML (<search> si <replace>)
// si aplica automat modificarile asupra fisierului.
func applyXMLDiff(originalContent string, llmResponse string, filePath string) (string, error) {

	// Expresie regulata care identifica toate blocurile
	// de forma:
	// <search> ... </search>
	//
	// (?s) activeaza modul "single line", astfel incat '.'
	// poate potrivi si caracterele newline.
	searchRegex := regexp.MustCompile(`(?s)<search>(.*?)</search>`)

	// FindAllStringSubmatchIndex returneaza pozitiile exacte
	// ale fiecarui bloc gasit.
	//
	// Avantajul utilizarii indexurilor este ca putem extrage
	// ulterior textul fara sa pierdem pozitia in raspunsul LLM.
	searchMatches := searchRegex.FindAllStringSubmatchIndex(llmResponse, -1)

	// Daca modelul nu a generat niciun bloc <search>,
	// nu exista modificari ce pot fi aplicate.
	if len(searchMatches) == 0 {
		return originalContent, fmt.Errorf("Nu am gasit niciun tag <search> valid")
	}

	// modifiedContent reprezinta versiunea curenta a fisierului.
	// Pe masura ce procesam fiecare diferenta,
	// aceasta variabila este actualizata succesiv.
	modifiedContent := originalContent

	// Se proceseaza fiecare bloc <search> gasit in raspuns.
	for i, matchLoc := range searchMatches {

		// matchLoc[2] si matchLoc[3] reprezinta inceputul
		// si sfarsitul continutului dintre tagurile <search>.
		searchText := strings.TrimSpace(llmResponse[matchLoc[2]:matchLoc[3]])

		// Eliminam eventualele taguri XML ramase accidental.
		searchText = cleanXMLTags(searchText)

		// matchLoc[1] indica pozitia imediat dupa </search>.
		// De aici incepe sectiunea care contine codul de inlocuire.
		replaceStart := matchLoc[1]

		// Implicit presupunem ca blocul de replace continua
		// pana la sfarsitul raspunsului.
		replaceEnd := len(llmResponse)

		// Daca exista un nou bloc <search>,
		// atunci codul de inlocuire trebuie sa se opreasca
		// inaintea acelui bloc.
		if i+1 < len(searchMatches) {
			replaceEnd = searchMatches[i+1][0]
		}

		// Extragem sectiunea completa care apartine
		// acestui bloc de modificare.
		rawReplaceBlock := llmResponse[replaceStart:replaceEnd]

		// Variabila in care va fi stocat codul final
		// ce trebuie introdus in fisier.
		var replaceText string

		// Expresie regulata pentru identificarea blocului
		// <replace> ... </replace>.
		replRegex := regexp.MustCompile(`(?s)<replace>(.*?)</replace>`)

		// Cautam daca modelul a respectat formatul cerut.
		replMatch := replRegex.FindStringSubmatch(rawReplaceBlock)

		// Daca blocul <replace> exista...
		if len(replMatch) > 0 {

			// ...extragem doar continutul sau.
			replaceText = replMatch[1]

		} else {

			// Uneori modelele LLM omit tagul <replace>.
			//
			// Pentru a face parserul mai robust,
			// se considera ca tot textul ramas dupa </search>
			// reprezinta codul de inserat.
			replaceText = rawReplaceBlock

			// Daca modelul a lasat accidental si tagul </diff>,
			// acesta este eliminat deoarece nu face parte din cod.
			if idx := strings.LastIndex(replaceText, "</diff>"); idx != -1 {
				replaceText = replaceText[:idx]
			}
		}

		// Eliminam orice tag XML ramas si pastram
		// doar codul sursa propriu-zis.
		replaceText = cleanXMLTags(replaceText)

		// Daca blocul <search> contine text,
		// inseamna ca trebuie modificat un fragment existent.
		if searchText != "" {

			// fuzzyReplace incearca sa gaseasca fragmentul
			// chiar daca exista diferente de spatii,
			// taburi sau formatari minore.
			newContent, err := fuzzyReplace(modifiedContent, searchText, replaceText)

			// Daca nu s-a putut realiza inlocuirea,
			// functia returneaza eroarea catre apelant.
			if err != nil {
				return modifiedContent, fmt.Errorf("Eroare la blocul #%d: %v", i+1, err)
			}

			// Actualizam continutul fisierului
			// cu versiunea modificata.
			modifiedContent = newContent

		} else {

			// Daca blocul <search> este gol,
			// interpretam operatia ca fiind o inserare
			// de cod nou.

			// Alegem automat cea mai potrivita pozitie
			// de inserare in functie de extensia fisierului.
			insertionPoint := getSmartInsertionPoint(modifiedContent, filePath)

			// Impartim fisierul in doua parti:
			// inainte si dupa punctul de inserare.
			prefix := modifiedContent[:insertionPoint]
			suffix := modifiedContent[insertionPoint:]

			// Inseram codul nou intre cele doua segmente,
			// pastrand o formatare lizibila.
			modifiedContent = prefix + "\n\n" + replaceText + "\n" + suffix
		}
	}

	// Dupa procesarea tuturor blocurilor,
	// returnam continutul final al fisierului.
	return modifiedContent, nil
}

// fuzzyReplace incearca sa inlocuiasca textul inteligent si aliniaza automat indentarea (Python-safe)
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

		// --- (Auto-Indent) ---
		lastNewline := strings.LastIndex(prefix, "\n")
		var originalIndent string
		if lastNewline != -1 {
			originalIndent = prefix[lastNewline+1:]
			// Verificam daca sunt doar spatii sau taburi
			isSpace := true
			for _, c := range originalIndent {
				if c != ' ' && c != '\t' {
					isSpace = false
					break
				}
			}
			if isSpace {
				// Taiem spatiile din prefix ca sa nu le dublam cu cele din replace
				prefix = prefix[:lastNewline+1]
			} else {
				originalIndent = ""
			}
		}

		replaceText := strings.Trim(replace, "\r\n") // Curatam enter-urile
		lines := strings.Split(replaceText, "\n")

		if originalIndent != "" && len(lines) > 0 {
			// Aflam indentarea pe care a halucinat-o AI-ul pe prima linie
			var aiIndent string
			for i, ch := range lines[0] {
				if ch != ' ' && ch != '\t' {
					aiIndent = lines[0][:i]
					break
				}
			}

			// Aplicam corectia (shift) pe toate liniile generate de AI
			for i, line := range lines {
				if strings.HasPrefix(line, aiIndent) {
					// inlocuim indentarea AI-ului cu indentarea originala a fisierului
					lines[i] = originalIndent + line[len(aiIndent):]
				} else if strings.TrimSpace(line) != "" {
					// Fallback pentru linii haotice: le aducem la nivelul de baza
					lines[i] = originalIndent + strings.TrimLeft(line, " \t")
				}
			}
			replaceText = strings.Join(lines, "\n")
		}

		return prefix + replaceText + suffix, nil
	}

	return content, fmt.Errorf("blocul de search nu a fost gasit (nici macar fuzzy)")
}
func getSmartInsertionPoint(content string, filePath string) int {
	ext := strings.ToLower(filepath.Ext(filePath))

	switch ext {
	case ".go":
		return findPointAfterImports(content)

	case ".html":
		// In HTML, inseram in interiorul ultimului bloc de script sau inainte de inchiderea body
		if idx := strings.LastIndex(content, "</script>"); idx != -1 {
			return idx
		}
		if idx := strings.LastIndex(content, "</body>"); idx != -1 {
			return idx
		}
		return len(content)

	case ".py":
		// In Python, cautam blocul principal 'if __name__ == "__main__":'
		// Vrem ca noile functii sa fie definite INAINTE de executia scriptului
		if idx := strings.Index(content, `if __name__ == "__main__":`); idx != -1 {
			return idx
		}
		// Daca nu are bloc main, adaugam la final
		return len(content)

	case ".css":
		// In CSS, noile reguli se adauga intotdeauna la finalul fisierului
		// pentru a respecta cascada (sa poata suprascrie stiluri anterioare)
		return len(content)

	default:
		// Pentru restul (.js, .json, .txt), finalul fisierului este cel mai sigur
		return len(content)
	}
}
func cleanXMLTags(text string) string {
	tagsToRemove := []string{
		"<diff>", "</diff>",
		"<search>", "</search>",
		"<replace>", "</replace>",
		"<search></search>",
		"<![CDATA[", "]]>",
	}

	for _, tag := range tagsToRemove {
		text = strings.ReplaceAll(text, tag, "")
	}

	// Taiem DOAR enter-urile de la inceput si sfarsit.
	// PASTRAM spatiile si tab-urile intacte pentru a nu strica Python-ul!
	text = strings.TrimLeft(text, "\r\n")
	text = strings.TrimRight(text, " \t\r\n")

	return text
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

// findPointAfterImports returneaza indexul unde se termina sectiunea de importuri.
func findPointAfterImports(content string) int {
	// 1. Cautam ultimul import (fie simplu, fie bloc)
	lastImportIdx := strings.LastIndex(content, "import")

	if lastImportIdx == -1 {
		// Daca nu exista importuri, cautam dupa declaratia 'package'
		pkgIdx := strings.Index(content, "package")
		if pkgIdx == -1 {
			return 0 // Fisier gol sau invalid
		}
		// Gasim sfarsitul liniei cu package
		lineEnd := strings.Index(content[pkgIdx:], "\n")
		if lineEnd == -1 {
			return len(content)
		}
		return pkgIdx + lineEnd + 1
	}

	// 2. Verificam daca importul este un bloc: import ( ... )
	rest := content[lastImportIdx:]
	// Verificam daca dupa "import" urmeaza o paranteza (ignorand spatiile)
	trimmedRest := strings.TrimSpace(rest[6:])
	if len(trimmedRest) > 0 && trimmedRest[0] == '(' {
		closeParen := strings.Index(rest, ")")
		if closeParen != -1 {
			return lastImportIdx + closeParen + 1
		}
	}

	// 3. Este un import pe o singura linie: import "fmt"
	lineEnd := strings.Index(rest, "\n")
	if lineEnd != -1 {
		return lastImportIdx + lineEnd + 1
	}

	return lastImportIdx + len("import")
}
