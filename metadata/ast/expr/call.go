package expr

import "github.com/viant/sqlx/metadata/ast/node"

type Call struct {
	X   node.Node
	Raw string
}
