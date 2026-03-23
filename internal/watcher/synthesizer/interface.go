// Package synthesizer provides auth synthesis strategies for the watcher package.
// It implements the Strategy pattern to support multiple auth sources:
// - ConfigSynthesizer: generates Auth entries from config API keys
// - FileSynthesizer: generates Auth entries from OAuth JSON files
package synthesizer
