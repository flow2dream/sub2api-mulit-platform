/** Normalize exported credentials without mutating the caller's document. */
export function normalizeKiroCredentials(raw: Record<string, unknown>): Record<string, unknown> {
  const result = { ...raw }
  const aliases: Record<string, string> = { accessToken: 'access_token', refreshToken: 'refresh_token', clientId: 'client_id', clientSecret: 'client_secret', profileArn: 'profile_arn', expiresAt: 'expires_at', authMethod: 'auth_method', authRegion: 'auth_region', apiRegion: 'api_region', apiKey: 'api_key', endpointFamily: 'endpoint_family' }
  for (const [alias, key] of Object.entries(aliases)) {
    if (!(key in result) && alias in result) result[key] = result[alias]
    delete result[alias]
  }
  if (typeof result.auth_method === 'string') result.auth_method = result.auth_method.trim().toLowerCase()
  if (result.auth_method === 'builder-id' || result.auth_method === 'iam') result.auth_method = 'idc'
  if (!result.endpoint_family && result.endpoint === 'ide') result.endpoint_family = 'q'
  if (!result.auth_region && result.region) result.auth_region = result.region
  if (!result.api_region && typeof result.profile_arn === 'string') {
    const region = result.profile_arn.split(':')[3]
    if (region) result.api_region = region
  }
  return result
}
