// Package configdraft validates the inert R09 configuration draft format.
package configdraft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	JSONLimit       = 256 << 10
	DockerfileLimit = 128 << 10
)

var (
	uuidPattern           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	credentialRefPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,255}$`)
	toolNamePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	sha256Pattern         = regexp.MustCompile(`^(?:sha256:)?[0-9a-f]{64}$`)
	rawSecretKeyPattern   = regexp.MustCompile(`(?i)(?:^|[,{[:space:]])["']?(?:token|access[_-]?token|api[_-]?key|password|passphrase|private[_-]?key|credential|credentials|authorization|proxy[_-]?authorization|x[_-]?api[_-]?key)["']?[[:space:]]*:`)
	tokenValuePattern     = regexp.MustCompile(`(?:ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{16,}|sk-[A-Za-z0-9]{20,})`)
	pemPattern            = regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`)
	dockerSecretDirective = regexp.MustCompile(`(?i)^\s*(?:ARG|ENV)\s+(?:TOKEN|ACCESS_TOKEN|API_KEY|PRIVATE_KEY|PASSWORD|PASSPHRASE|AUTHORIZATION|X_API_KEY)(?:\s|=|$)`)
	dockerSecretHeader    = regexp.MustCompile(`(?i)(?:Authorization|Proxy-Authorization|X-Api-Key|Api-Key)\s*:`)
)

type Diagnostic struct {
	Code    string `json:"code"`
	Pointer string `json:"pointer"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}
type Validation struct {
	Valid        bool         `json:"valid"`
	Diagnostics  []Diagnostic `json:"diagnostics"`
	EffectStatus string       `json:"effectStatus"`
}
type Result struct{ Validation Validation }
type location struct{ line, column int }

func Validate(rawJSON, dockerfile string) Result {
	return ValidateWithBuildContext(rawJSON, dockerfile, "")
}

func ValidateWithBuildContext(rawJSON, dockerfile, manifestRevision string) Result {
	result := Result{Validation: Validation{Diagnostics: []Diagnostic{}, EffectStatus: "none"}}
	add := func(code, pointer, message string, at location) {
		result.Validation.Diagnostics = append(result.Validation.Diagnostics, Diagnostic{code, pointer, at.line, at.column, message})
	}
	if !utf8.ValidString(rawJSON) || len(rawJSON) > JSONLimit {
		add("json_size", "", "JSON должен быть UTF-8 и не превышать 256 KiB.", location{1, 1})
		return result
	}
	if !utf8.ValidString(dockerfile) || len(dockerfile) > DockerfileLimit {
		add("dockerfile_size", "/dockerfile", "Dockerfile должен быть UTF-8 и не превышать 128 KiB.", location{1, 1})
	}
	scanRawSecrets(rawJSON, "", add)
	scanDockerfile(dockerfile, add)
	positions, duplicate, syntax := inspect([]byte(rawJSON))
	if syntax != nil {
		add("invalid_json", nearestPointer(positions), "JSON имеет некорректный синтаксис.", syntaxLocation([]byte(rawJSON), syntax))
		return result
	}
	if duplicate != nil {
		add("duplicate_field", duplicate.pointer, "Поле JSON не должно повторяться.", duplicate.at)
	}
	var root map[string]any
	if json.Unmarshal([]byte(rawJSON), &root) != nil || root == nil {
		add("invalid_type", "", "Корень конфигурации должен быть объектом.", location{1, 1})
		return result
	}
	scanForbidden(root, "", positions, add)
	checkObject(root, map[string]bool{"schemaVersion": true, "name": true, "engine": true, "deployment": true, "profile": true}, []string{"schemaVersion", "name", "engine", "deployment", "profile"}, "", positions, add)
	if version, ok := root["schemaVersion"].(float64); !ok || version != 2 {
		add("invalid_schema_version", "/schemaVersion", "Поддерживается только schemaVersion 2.", pos(positions, "/schemaVersion"))
	}
	validateText(root["name"], "/name", 1, 120, positions, add)
	engine, ok := root["engine"].(string)
	if !ok || (engine != "cursor" && engine != "codex") {
		add("invalid_engine", "/engine", "engine должен быть cursor или codex.", pos(positions, "/engine"))
	}
	validateDeployment(root["deployment"], positions, add)
	validateProfile(root["profile"], positions, add)
	if manifestRevision != "" {
		if deployment, ok := root["deployment"].(map[string]any); ok {
			kind, _ := deployment["kind"].(string)
			revision, _ := deployment["buildContextRevision"].(string)
			if kind == "external" || (kind == "managed" && revision != manifestRevision) {
				add("build_context_binding", "/buildContextManifest/revision", "Build context manifest должен соответствовать managed deployment.buildContextRevision.", location{1, 1})
			}
		}
	}
	result.Validation.Valid = len(result.Validation.Diagnostics) == 0
	return result
}

func HasSecretDiagnostics(result Result) bool {
	for _, d := range result.Validation.Diagnostics {
		if strings.HasPrefix(d.Code, "secret_") {
			return true
		}
	}
	return false
}

func validateDeployment(value any, positions map[string]location, add func(string, string, string, location)) {
	object, ok := value.(map[string]any)
	if !ok {
		add("invalid_type", "/deployment", "deployment должен быть объектом.", pos(positions, "/deployment"))
		return
	}
	kind, _ := object["kind"].(string)
	if kind == "managed" {
		checkObject(object, map[string]bool{"kind": true, "hostId": true, "dockerfileRevision": true, "buildContextRevision": true, "registryCredentialRef": true}, []string{"kind", "hostId", "dockerfileRevision"}, "/deployment", positions, add)
		validateUUID(object["hostId"], "/deployment/hostId", positions, add)
		validateUUID(object["dockerfileRevision"], "/deployment/dockerfileRevision", positions, add)
		if v, exists := object["buildContextRevision"]; exists {
			validateUUID(v, "/deployment/buildContextRevision", positions, add)
		}
		if v, exists := object["registryCredentialRef"]; exists {
			validateCredentialRef(v, "/deployment/registryCredentialRef", positions, add)
		}
		return
	}
	if kind == "external" {
		checkObject(object, map[string]bool{"kind": true, "endpointRef": true}, []string{"kind", "endpointRef"}, "/deployment", positions, add)
		validateCredentialRef(object["endpointRef"], "/deployment/endpointRef", positions, add)
		return
	}
	checkObject(object, map[string]bool{"kind": true}, []string{"kind"}, "/deployment", positions, add)
	add("invalid_union", "/deployment/kind", "deployment.kind должен быть managed или external.", pos(positions, "/deployment/kind"))
}

func validateProfile(value any, positions map[string]location, add func(string, string, string, location)) {
	profile, ok := value.(map[string]any)
	if !ok {
		add("invalid_type", "/profile", "profile должен быть объектом.", pos(positions, "/profile"))
		return
	}
	checkObject(profile, map[string]bool{"mode": true, "basePrompt": true, "instructions": true, "mcp": true, "access": true}, []string{"mode", "basePrompt", "instructions", "mcp", "access"}, "/profile", positions, add)
	mode, _ := profile["mode"].(string)
	if mode != "universal" && mode != "specialized" {
		add("invalid_profile_mode", "/profile/mode", "profile.mode должен быть universal или specialized.", pos(positions, "/profile/mode"))
	}
	validateText(profile["basePrompt"], "/profile/basePrompt", 0, 65536, positions, add)
	validateStringArray(profile["instructions"], "/profile/instructions", 128, 16384, positions, add)
	mcp, ok := profile["mcp"].([]any)
	if !ok {
		add("invalid_type", "/profile/mcp", "mcp должен быть массивом.", pos(positions, "/profile/mcp"))
	} else if len(mcp) > 64 {
		add("too_many_items", "/profile/mcp", "mcp содержит слишком много элементов.", pos(positions, "/profile/mcp"))
	} else {
		for i, item := range mcp {
			validateMCP(item, fmt.Sprintf("/profile/mcp/%d", i), positions, add)
		}
	}
	access, ok := profile["access"].(map[string]any)
	if !ok {
		add("invalid_type", "/profile/access", "access должен быть объектом.", pos(positions, "/profile/access"))
		return
	}
	checkObject(access, map[string]bool{"default": true, "nativeTools": true}, []string{"default", "nativeTools"}, "/profile/access", positions, add)
	if access["default"] != "deny" {
		add("invalid_access_default", "/profile/access/default", "access.default должен быть deny.", pos(positions, "/profile/access/default"))
	}
	validateStringArray(access["nativeTools"], "/profile/access/nativeTools", 128, 128, positions, add)
}

func validateMCP(value any, pointer string, positions map[string]location, add func(string, string, string, location)) {
	object, ok := value.(map[string]any)
	if !ok {
		add("invalid_type", pointer, "mcp item должен быть объектом.", pos(positions, pointer))
		return
	}
	checkObject(object, map[string]bool{"serverId": true, "transport": true, "uri": true, "credentialRef": true, "tools": true}, []string{"serverId", "transport", "uri", "tools"}, pointer, positions, add)
	validateText(object["serverId"], pointer+"/serverId", 1, 128, positions, add)
	transport, _ := object["transport"].(string)
	if transport != "stdio" && transport != "streamable-http" {
		add("invalid_transport", pointer+"/transport", "transport должен быть stdio или streamable-http.", pos(positions, pointer+"/transport"))
	}
	validateText(object["uri"], pointer+"/uri", 1, 2048, positions, add)
	if v, exists := object["credentialRef"]; exists {
		validateCredentialRef(v, pointer+"/credentialRef", positions, add)
	}
	tools, ok := object["tools"].([]any)
	if !ok {
		add("invalid_type", pointer+"/tools", "tools должен быть массивом.", pos(positions, pointer+"/tools"))
		return
	}
	if len(tools) > 256 {
		add("too_many_items", pointer+"/tools", "tools содержит слишком много элементов.", pos(positions, pointer+"/tools"))
		return
	}
	for i, item := range tools {
		p := fmt.Sprintf("%s/tools/%d", pointer, i)
		tool, ok := item.(map[string]any)
		if !ok {
			add("invalid_type", p, "tool должен быть объектом.", pos(positions, p))
			continue
		}
		checkObject(tool, map[string]bool{"name": true, "schemaHash": true}, []string{"name", "schemaHash"}, p, positions, add)
		name, nok := tool["name"].(string)
		if !nok || !toolNamePattern.MatchString(name) {
			add("invalid_tool_name", p+"/name", "Некорректное имя tool.", pos(positions, p+"/name"))
		}
		hash, hok := tool["schemaHash"].(string)
		if !hok || !sha256Pattern.MatchString(hash) {
			add("invalid_ref", p+"/schemaHash", "schemaHash должен быть SHA-256.", pos(positions, p+"/schemaHash"))
		}
	}
}

func checkObject(object map[string]any, allowed map[string]bool, required []string, base string, positions map[string]location, add func(string, string, string, location)) {
	for key, value := range object {
		pointer := base + "/" + escape(key)
		if !allowed[key] {
			add("unknown_field", pointer, "Неизвестное поле конфигурации.", pos(positions, pointer))
		} else if value == nil {
			add("null_field", pointer, "null не допускается.", pos(positions, pointer))
		}
	}
	for _, key := range required {
		if _, exists := object[key]; !exists {
			add("required_field", base+"/"+escape(key), "Обязательное поле отсутствует.", pos(positions, base))
		}
	}
}
func validateText(value any, pointer string, min, max int, positions map[string]location, add func(string, string, string, location)) {
	text, ok := value.(string)
	if !ok || len(text) < min || len(text) > max || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
		add("invalid_text", pointer, "Некорректное текстовое поле.", pos(positions, pointer))
	}
}
func validateStringArray(value any, pointer string, maxItems, maxText int, positions map[string]location, add func(string, string, string, location)) {
	items, ok := value.([]any)
	if !ok {
		add("invalid_type", pointer, "Ожидается массив строк.", pos(positions, pointer))
		return
	}
	if len(items) > maxItems {
		add("too_many_items", pointer, "Слишком много элементов.", pos(positions, pointer))
		return
	}
	for i, item := range items {
		validateText(item, fmt.Sprintf("%s/%d", pointer, i), 1, maxText, positions, add)
	}
}
func validateUUID(value any, pointer string, positions map[string]location, add func(string, string, string, location)) {
	text, ok := value.(string)
	if !ok || !uuidPattern.MatchString(text) {
		add("invalid_ref", pointer, "Требуется UUID reference.", pos(positions, pointer))
	}
}
func validateCredentialRef(value any, pointer string, positions map[string]location, add func(string, string, string, location)) {
	text, ok := value.(string)
	if !ok || !credentialRefPattern.MatchString(text) || strings.Contains(text, "//") || strings.HasSuffix(text, "/") {
		add("invalid_ref", pointer, "Требуется безопасная reference-строка.", pos(positions, pointer))
	}
}

var forbiddenKeys = map[string]bool{"authorization": true, "proxyauthorization": true, "xapikey": true, "apikey": true, "token": true, "accesstoken": true, "privatekey": true, "password": true, "passphrase": true, "credential": true, "credentials": true}

func normalizeKey(value string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(value))
}
func scanForbidden(value any, base string, positions map[string]location, add func(string, string, string, location)) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			pointer := base + "/" + escape(key)
			if forbiddenKeys[normalizeKey(key)] {
				add("secret_field", pointer, "Inline secret field is not allowed; use credentialRef.", pos(positions, pointer))
			}
			scanForbidden(child, pointer, positions, add)
		}
	case []any:
		for index, child := range typed {
			scanForbidden(child, fmt.Sprintf("%s/%d", base, index), positions, add)
		}
	case string:
		for _, pattern := range []*regexp.Regexp{pemPattern, tokenValuePattern} {
			if pattern.MatchString(typed) {
				add("secret_content", base, "Secret-like content is not allowed; use credentialRef.", pos(positions, base))
				break
			}
		}
	}
}
func scanRawSecrets(value, pointer string, add func(string, string, string, location)) {
	for _, pattern := range []*regexp.Regexp{rawSecretKeyPattern, pemPattern, tokenValuePattern} {
		for _, match := range pattern.FindAllStringIndex(value, -1) {
			add("secret_content", pointer, "Secret-like content is not allowed; use credentialRef.", offsetLocation([]byte(value), int64(match[0])))
		}
	}
	scanDecodedStringTokens(value, add)
}
func scanDecodedStringTokens(value string, add func(string, string, string, location)) {
	for start := 0; start < len(value); start++ {
		if value[start] != '"' {
			continue
		}
		escaped := false
		end := start + 1
		for ; end < len(value); end++ {
			switch {
			case escaped:
				escaped = false
			case value[end] == '\\':
				escaped = true
			case value[end] == '"':
				goto complete
			}
		}
		break
	complete:
		var decoded string
		if json.Unmarshal([]byte(value[start:end+1]), &decoded) != nil {
			start = end
			continue
		}
		next := end + 1
		for next < len(value) && (value[next] == ' ' || value[next] == '\t' || value[next] == '\r' || value[next] == '\n') {
			next++
		}
		if next < len(value) && value[next] == ':' && forbiddenKeys[normalizeKey(decoded)] {
			add("secret_field", "/"+escape(normalizeKey(decoded)), "Inline secret field is not allowed; use credentialRef.", offsetLocation([]byte(value), int64(start)))
		} else if pemPattern.MatchString(decoded) || tokenValuePattern.MatchString(decoded) {
			add("secret_content", "", "Secret-like content is not allowed; use credentialRef.", offsetLocation([]byte(value), int64(start)))
		}
		start = end
	}
}
func scanDockerfile(value string, add func(string, string, string, location)) {
	for index, line := range strings.Split(value, "\n") {
		column := -1
		for _, pattern := range []*regexp.Regexp{dockerSecretDirective, dockerSecretHeader, pemPattern, tokenValuePattern} {
			if match := pattern.FindStringIndex(line); match != nil {
				column = match[0]
				break
			}
		}
		if column >= 0 {
			add("secret_content", "/dockerfile", "Secret-like Dockerfile content is not allowed; use references.", location{index + 1, column + 1})
		}
	}
}

type duplicateInfo struct {
	pointer string
	at      location
}

type locatedError struct{ offset int64 }

func (e *locatedError) Error() string { return "trailing JSON value" }

func inspect(raw []byte) (map[string]location, *duplicateInfo, error) {
	positions := map[string]location{"": {1, 1}}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var duplicate *duplicateInfo
	var walk func(string) error
	walk = func(pointer string) error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch delimiter := token.(type) {
		case json.Delim:
			switch delimiter {
			case '{':
				seen := map[string]bool{}
				for decoder.More() {
					before := decoder.InputOffset()
					keyToken, err := decoder.Token()
					if err != nil {
						return err
					}
					key, ok := keyToken.(string)
					if !ok {
						return fmt.Errorf("object key")
					}
					child := pointer + "/" + escape(key)
					positions[child] = offsetLocation(raw, keyStart(raw, before))
					if seen[key] && duplicate == nil {
						duplicate = &duplicateInfo{child, positions[child]}
					}
					seen[key] = true
					if err := walk(child); err != nil {
						return err
					}
				}
				_, err = decoder.Token()
				return err
			case '[':
				index := 0
				for decoder.More() {
					child := fmt.Sprintf("%s/%d", pointer, index)
					positions[child] = offsetLocation(raw, keyStart(raw, decoder.InputOffset()))
					if err := walk(child); err != nil {
						return err
					}
					index++
				}
				_, err = decoder.Token()
				return err
			default:
				return fmt.Errorf("invalid delimiter")
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return positions, duplicate, err
	}
	trailingOffset := keyStart(raw, decoder.InputOffset())
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			err = &locatedError{offset: trailingOffset}
		}
		return positions, duplicate, err
	}
	return positions, duplicate, nil
}
func escape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
func pos(positions map[string]location, pointer string) location {
	if value, ok := positions[pointer]; ok {
		return value
	}
	return location{1, 1}
}
func offsetLocation(raw []byte, offset int64) location {
	line, column := 1, 1
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(raw)) {
		offset = int64(len(raw))
	}
	for _, b := range raw[:offset] {
		if b == '\n' {
			line, column = line+1, 1
		} else {
			column++
		}
	}
	return location{line, column}
}

func keyStart(raw []byte, offset int64) int64 {
	for offset < int64(len(raw)) {
		switch raw[offset] {
		case ' ', '\t', '\r', '\n', ',':
			offset++
		default:
			return offset
		}
	}
	return offset
}
func syntaxLocation(raw []byte, err error) location {
	if syntax, ok := err.(*json.SyntaxError); ok {
		return offsetLocation(raw, syntax.Offset-1)
	}
	if located, ok := err.(*locatedError); ok {
		return offsetLocation(raw, located.offset)
	}
	return offsetLocation(raw, int64(len(raw)))
}
func nearestPointer(positions map[string]location) string {
	best := ""
	bestLine, bestColumn := 1, 1
	for pointer, at := range positions {
		if at.line > bestLine || (at.line == bestLine && at.column >= bestColumn) {
			best, bestLine, bestColumn = pointer, at.line, at.column
		}
	}
	return best
}
