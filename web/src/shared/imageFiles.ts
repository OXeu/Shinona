const supportedImageTypes = ['image/png', 'image/jpeg', 'image/webp']

export const imageFileAccept = supportedImageTypes.join(',')

export function isSupportedImageType(mediaType: string): boolean {
  return supportedImageTypes.includes(mediaType)
}
