export function routeRedirect(to, isOnline) {
  if (to.name === 'invite' && !isOnline) {
    return {
      name: 'login',
      query: { invite: String(to.params?.token || '') },
    }
  }

  if (to.meta?.requiresAuth && !isOnline) {
    return { name: 'login' }
  }

  if (to.name === 'login' && isOnline) {
    const rawInvite = to.query?.invite
    const invite = Array.isArray(rawInvite) ? rawInvite[0] : rawInvite
    if (invite) {
      return { name: 'invite', params: { token: String(invite) } }
    }
    return { name: 'farm' }
  }

  return null
}
