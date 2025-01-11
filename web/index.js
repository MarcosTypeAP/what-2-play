const FAVORITE_FRIENDS_COOKIE_NAME = 'favorite-friends'

/**
 * @param {HTMLElement} el
 */
function favoriteFriendToggle(el) {
    const friendSteamID = el.getAttribute('data-friend-steamid')
    if (friendSteamID === null) {
        throw new Error("missing 'data-friend-steamid'")
    }
    const userSteamID = el.getAttribute('data-user-steamid')
    if (userSteamID === null) {
        throw new Error("missing 'data-user-steamid'")
    }

    const cookieName = FAVORITE_FRIENDS_COOKIE_NAME + '-' + userSteamID

    const favorites = document.cookie
        .split(';')
        .find((part) => part.trimStart().startsWith(cookieName + '='))
        ?.trimEnd()
        .split('=')[1]
        .split(',') ?? []

    let isFavorite = favorites.includes(friendSteamID)

    /** @type {string} */
    let newFavorites

    if (isFavorite) {
        newFavorites = favorites.filter((id) => id != friendSteamID).join(',')
        // @ts-ignore
        htmx.closest(el, 'li').classList.remove('favorite')
    } else {
        newFavorites = favorites.concat(friendSteamID).join(',')
        // @ts-ignore
        htmx.closest(el, 'li').classList.add('favorite')
    }

    document.cookie = cookieName + '=' + newFavorites + '; SameSite=strict'
}
