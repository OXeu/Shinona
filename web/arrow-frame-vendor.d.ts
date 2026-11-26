declare module 'virtual:arrow-compiler' {
  import type { TemplateDescriptor } from 'virtual:arrow-protocol'

  export function preprocessModule(source: string, path: string): {
    code: string
    descriptors: TemplateDescriptor[]
  }
}

declare module 'virtual:arrow-renderer' {
  import type { SandboxedEventPayload, SerializedNode, VmPatch } from 'virtual:arrow-protocol'

  export class HostRenderer {
    constructor(options: {
      mountPoint: Element
      onEvent: (handlerId: string, payload: SandboxedEventPayload) => Promise<void>
      onError: (error: Error | string) => void
    })
    render(tree: SerializedNode): void
    applyPatches(patches: VmPatch[]): void
    destroy(): void
  }
}

declare module 'virtual:arrow-vm' {
  import type { HostToVmMessage, TemplateDescriptor, VmToHostMessage } from 'virtual:arrow-protocol'

  export interface VmRunner {
    dispatch(message: HostToVmMessage): Promise<void>
    destroy(): void
  }

  export function createVmRunner(options: {
    compiled: {
      entryPath: string
      modules: Record<string, string>
      descriptors: Record<string, TemplateDescriptor>
    }
    onMessage: (message: VmToHostMessage) => void
  }): Promise<VmRunner>
}
