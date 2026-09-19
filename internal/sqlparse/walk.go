package sqlparse

import (
	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// walk visits every Node in a parse tree, depth first.
//
// The parse tree is a protobuf whose node type is a giant one-of with well over
// two hundred variants. Hand-writing a visitor for it would be several thousand
// lines that go stale the moment libpg_query adds a node type, so this reflects
// over the message instead: any nested message that is a *pg.Node is visited,
// and everything else is recursed into. It is slower than a generated visitor
// and irrelevantly so — the trees here are one statement, not one schema.
func walk(root *pg.Node, visit func(*pg.Node)) {
	if root == nil {
		return
	}
	visit(root)
	walkMessage(root.ProtoReflect(), visit)
}

// WalkTree visits every Node in a parsed statement.
func WalkTree(st *Statement, visit func(*pg.Node)) {
	if st == nil || st.tree == nil {
		return
	}
	for _, raw := range st.tree.Stmts {
		walk(raw.Stmt, visit)
	}
}

func walkMessage(msg protoreflect.Message, visit func(*pg.Node)) {
	msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
					visitValueMessage(list.Get(i).Message(), visit)
				}
			}
		case fd.IsMap():
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				if fd.MapValue().Kind() == protoreflect.MessageKind {
					visitValueMessage(mv.Message(), visit)
				}
				return true
			})
		case fd.Kind() == protoreflect.MessageKind, fd.Kind() == protoreflect.GroupKind:
			visitValueMessage(v.Message(), visit)
		}
		return true
	})
}

func visitValueMessage(m protoreflect.Message, visit func(*pg.Node)) {
	if node, ok := m.Interface().(*pg.Node); ok {
		if node != nil && !proto.Equal(node, &pg.Node{}) {
			visit(node)
		}
		walkMessage(m, visit)
		return
	}
	walkMessage(m, visit)
}
