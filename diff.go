package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

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
		searchText = cleanXMLTags(searchText)
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
			// AI-ul a pus <replace>
			replaceText = replMatch[1]
		} else {
			// 2.  A uitat <replace>.
			// Luăm ABSOLUT TOT ce a scris după </search>
			replaceText = rawReplaceBlock

			// Dacă din întâmplare a lăsat tag-ul de închidere </diff> la final, îl decupăm ca să rămână doar codul
			if idx := strings.LastIndex(replaceText, "</diff>"); idx != -1 {
				replaceText = replaceText[:idx]
			}
		}

		//  Trecem codul extras prin filtrul tău de curățare a tag-urilor
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

		// --- (Auto-Indent) ---
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
		"<![CDATA[", "]]>",
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
