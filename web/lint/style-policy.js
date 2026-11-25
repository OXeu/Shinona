import postcss from 'postcss'
import { parse as parseLess } from 'postcss-less'

export const outlineOffsetMessage = 'The outline-offset property is forbidden.'

export function outlineOffsetDeclarations(stylesheet, from, isLess = /\.less$/iu.test(from)) {
  const parse = isLess ? parseLess : postcss.parse
  const declarations = []
  parse(stylesheet, { from }).walkDecls(/^outline-offset$/iu, (declaration) => {
    declarations.push(declaration.source.start)
  })
  return declarations
}

function isOutlineOffset(name) {
  return typeof name === 'string' && (name === 'outlineOffset' || name.toLowerCase() === 'outline-offset')
}

export const noOutlineOffset = {
  meta: {
    type: 'problem',
    schema: [],
    messages: {
      forbidden: outlineOffsetMessage,
      invalidCss: '{{ message }}',
    },
  },
  create(context) {
    const source = context.sourceCode
    const parser = source.parserServices

    function checkStylesheet(stylesheet, start, isLess) {
      try {
        for (const { offset } of outlineOffsetDeclarations(stylesheet, undefined, isLess)) {
          context.report({ loc: source.getLocFromIndex(start + offset), messageId: 'forbidden' })
        }
      } catch (error) {
        if (error.name !== 'CssSyntaxError') throw error
        context.report({ loc: source.getLocFromIndex(start), messageId: 'invalidCss', data: { message: error.reason } })
      }
    }

    function checkName(node, computed = false) {
      const name = node.type === 'Identifier' && !computed ? node.name : node.value
      if (isOutlineOffset(name)) {
        context.report({ node, messageId: 'forbidden' })
      }
    }

    const script = {
      Property({ key, computed }) {
        checkName(key, computed)
      },
      MemberExpression({ property, computed }) {
        checkName(property, computed)
      },
      CallExpression({ callee, arguments: args }) {
        if (callee.type !== 'MemberExpression' || !args.length) return
        const method = callee.computed ? callee.property.value : callee.property.name
        if (method === 'setProperty' && args[0].type === 'Literal') checkName(args[0])
      },
    }

    if (!parser.defineTemplateBodyVisitor) return script

    return parser.defineTemplateBodyVisitor(
      {
        ...script,
        VAttribute(node) {
          const name = node.directive ? node.key.argument?.name : node.key.name
          if (name !== 'style' || !node.value) return
          const value = node.directive ? node.value.expression : node.value
          if (typeof value?.value === 'string') {
            checkStylesheet(value.value, value.range[0] + 1)
          }
        },
      },
      {
        ...script,
        Program() {
          for (const block of parser.getDocumentFragment()?.children ?? []) {
            if (block.type !== 'VElement' || block.name !== 'style' || !block.endTag) continue
            const start = block.startTag.range[1]
            const isLess = block.startTag.attributes.some((attribute) =>
              !attribute.directive && attribute.key.name === 'lang' && attribute.value?.value === 'less')
            checkStylesheet(source.text.slice(start, block.endTag.range[0]), start, isLess)
          }
        },
      },
    )
  },
}
