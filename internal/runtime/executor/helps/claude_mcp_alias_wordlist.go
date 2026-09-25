package helps

import (
	_ "embed"
	"strings"
)

//go:embed claude_bip39_words.txt
var rawBIP39EnglishWords string

var claudeMCPAliasEnglishWords = strings.Fields(rawBIP39EnglishWords)
