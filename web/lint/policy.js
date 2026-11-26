const feedbackRoles = new Set(['alert', 'alertdialog', 'status', 'log'])
const dingPackage = /^@yusufceng1z\/ding(?:-|\/|$)/u

export const toastOnly = {
  meta: {
    type: 'problem',
    schema: [],
    messages: {
      inline: 'Use notify for transient feedback. Live feedback belongs to the app toaster.',
    },
  },
  create(context) {
    return context.sourceCode.parserServices.defineTemplateBodyVisitor({
      VElement(node) {
        if (node.name === 'output') context.report({ node, messageId: 'inline' })
      },
      VAttribute(node) {
        const name = node.directive ? node.key.argument?.name : node.key.name
        const role = node.directive ? node.value?.expression?.value : node.value?.value
        if (name === 'aria-live' || (name === 'role' && typeof role === 'string' && role.split(/\s+/u).some((word) => feedbackRoles.has(word)))) {
          context.report({ node, messageId: 'inline' })
        }
      },
    })
  },
}

export const notificationGateway = {
  meta: {
    type: 'problem',
    schema: [],
    messages: { gateway: 'Import notifications through src/shared/notifications.ts.' },
  },
  create(context) {
    function check(node) {
      if (typeof node?.value === 'string' && dingPackage.test(node.value)) {
        context.report({ node, messageId: 'gateway' })
      }
    }

    return {
      ImportDeclaration: (node) => check(node.source),
      ExportNamedDeclaration: (node) => check(node.source),
      ExportAllDeclaration: (node) => check(node.source),
      ImportExpression: (node) => check(node.source),
    }
  },
}

const templateControlTags = new Set(['template', 'slot'])

export const componentTemplates = {
  meta: {
    type: 'problem',
    schema: [],
    messages: { raw: 'Use a PascalCase component in feature templates. Native elements belong in src/shared/ui.' },
  },
  create(context) {
    return context.sourceCode.parserServices.defineTemplateBodyVisitor({
      VElement(node) {
        // rawName preserves the distinction between <UiButton> and native or custom lowercase tags.
        const name = node.rawName
        if (!templateControlTags.has(name) && !/^[A-Z][A-Za-z0-9]*$/u.test(name)) {
          context.report({ node: node.startTag, messageId: 'raw' })
        }
      },
    })
  },
}

const blockedElements = new Map([
  ['select', 'UiChoiceDropdown'],
  ['option', 'UiChoiceDropdown'],
  ['optgroup', 'UiChoiceDropdown'],
  ['dialog', 'UiDialog'],
  ['alert', 'notify'],
])
const browserDialogs = new Set(['alert', 'confirm', 'prompt'])
const browserGlobals = new Set(['window', 'self', 'globalThis'])

export const nativeControls = {
  meta: {
    type: 'problem',
    schema: [{ type: 'object', properties: { allowDialog: { type: 'boolean' } }, additionalProperties: false }],
    messages: {
      element: 'Replace <{{ name }}> with {{ replacement }}.',
      browserDialog: 'Use notify for messages and UiDialog for confirmation or input.',
    },
  },
  create(context) {
    const { allowDialog = false } = context.options[0] ?? {}
    return context.sourceCode.parserServices.defineTemplateBodyVisitor({
      VElement(node) {
        const name = node.name
        const replacement = blockedElements.get(name)
        if (replacement && !(allowDialog && name === 'dialog')) {
          context.report({ node: node.startTag, messageId: 'element', data: { name, replacement } })
        }
      },
      CallExpression({ callee }) {
        const direct = callee.type === 'Identifier' && browserDialogs.has(callee.name)
        const global = callee.type === 'MemberExpression' && callee.object.type === 'Identifier' &&
          browserGlobals.has(callee.object.name) && (callee.computed
            ? callee.property.type === 'Literal' && browserDialogs.has(callee.property.value)
            : callee.property.type === 'Identifier' && browserDialogs.has(callee.property.name))
        if (direct || global) context.report({ node: callee, messageId: 'browserDialog' })
      },
    })
  },
}
