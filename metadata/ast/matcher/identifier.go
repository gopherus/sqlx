package matcher

import (
	"github.com/viant/parsly"
)

type identifier struct{}

//Match matches a string
func (n *identifier) Match(cursor *parsly.Cursor) (matched int) {
	input := cursor.Input
	pos := cursor.Pos
	if startsWithCharacter := IsLetter(input[pos]); startsWithCharacter {
		pos++
		matched++
	} else {
		return 0
	}

	size := len(input)
	for i := pos; i < size; i++ {
		switch input[i] {
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', '_':
			matched++
			continue
		default:
			if IsLetter(input[i]) {
				matched++
				continue
			}
			isLast := i+1 == len(input)
			if !isLast && !(isWhitespace(input[i]) || input[i] == '(' || input[i] == ',') {
				return 0
			}
			return matched
		}
	}

	return matched
}

func IsLetter(b byte) bool {
	if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') {
		return false
	}
	return true
}

//NewIdentifier creates a string matcher
func NewIdentifier() *identifier {
	return &identifier{}
}
