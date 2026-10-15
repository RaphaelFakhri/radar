// @monaco-editor/react 4.7 and @monaco-editor/loader still type monaco through this
// pre-0.57 path, which monaco-editor 0.57's exports map no longer serves.
declare module 'monaco-editor/esm/vs/editor/editor.api' {
  export * from 'monaco-editor/editor/editor.api'
}

declare module 'monaco-editor/languages/definitions/yaml/register'
declare module 'monaco-editor/languages/definitions/xml/register'
declare module 'monaco-editor/languages/definitions/html/register'
declare module 'monaco-editor/languages/definitions/css/register'
declare module 'monaco-editor/languages/definitions/javascript/register'
declare module 'monaco-editor/languages/definitions/typescript/register'
declare module 'monaco-editor/languages/definitions/python/register'
declare module 'monaco-editor/languages/definitions/shell/register'
declare module 'monaco-editor/languages/definitions/markdown/register'
declare module 'monaco-editor/languages/definitions/ini/register'
declare module 'monaco-editor/languages/definitions/dockerfile/register'
declare module 'monaco-editor/editor/contrib/find/browser/findController.js'
declare module 'monaco-editor/editor/contrib/folding/browser/folding.js'
declare module 'monaco-editor/editor/contrib/format/browser/formatActions.js'
declare module 'monaco-editor/editor/contrib/gotoError/browser/gotoError.js'
declare module 'monaco-editor/editor/contrib/hover/browser/hoverContribution.js'
declare module 'monaco-editor/editor/contrib/suggest/browser/suggestController.js'
