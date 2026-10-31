package online.phimnet.tv.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.navigation.NavHostController
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.rememberNavController
import androidx.navigation.toRoute
import androidx.tv.material3.MaterialTheme
import kotlinx.serialization.Serializable
import online.phimnet.tv.LocalGraph
import online.phimnet.tv.api.WatchKind
import online.phimnet.tv.ui.browse.BrowseScreen
import online.phimnet.tv.ui.common.Tab
import online.phimnet.tv.ui.detail.DetailScreen
import online.phimnet.tv.ui.home.HomeScreen
import online.phimnet.tv.ui.link.LinkScreen
import online.phimnet.tv.ui.player.PlayerScreen
import online.phimnet.tv.ui.settings.SettingsScreen
import online.phimnet.tv.ui.welcome.WelcomeScreen

/** Where an unpaired TV starts; the pair-first screen on a members-only server. */
@Serializable data object WelcomeRoute

@Serializable data object HomeRoute

/** The discovery grid. [type] is "movie" / "tv" for the Movies / Series tabs; null is search. */
@Serializable data class BrowseRoute(val type: String? = null)

@Serializable data class DetailRoute(val id: Long)

@Serializable data class PlayerRoute(val kind: WatchKind, val id: Long)

@Serializable data object LinkRoute

/**
 * [frontDoor]: opened from the pair-first screen, so it shows no tabs — they
 * would lead past the pairing a members-only server asks for.
 */
@Serializable data class SettingsRoute(val frontDoor: Boolean = false)

@Composable
fun AppNav() {
    val graph = LocalGraph.current
    val nav = rememberNavController()
    val onTab: (Tab) -> Unit = { nav.openTab(it) }

    val prefs by graph.settings.prefs.collectAsStateWithLifecycle()
    val token = prefs?.token
    val server = prefs?.serverUrl

    // A paired TV goes straight to Home; an unpaired one asks the server first
    // (WelcomeScreen), which steps aside at once unless sign-in is required.
    val start: Any = remember { if (token == null) WelcomeRoute else HomeRoute }

    // Losing the pairing — signing out, changing the server, or a token the
    // server revoked — goes back to the front door, whatever was on screen, so
    // a members-only server is never browsed unpaired.
    var wasSignedIn by remember { mutableStateOf(token != null) }
    LaunchedEffect(token != null) {
        if (wasSignedIn && token == null) {
            nav.navigate(WelcomeRoute) { popUpTo(nav.graph.id) { inclusive = true } }
        }
        wasSignedIn = token != null
    }

    // A token the server no longer honours (revoked from the account page, or
    // the server's secret rotated) reads as signed out on /me. Forget it, so the
    // app never claims a sign-in the server has already ended. Offline says
    // nothing either way.
    LaunchedEffect(token, server) {
        if (token == null) return@LaunchedEffect
        val me = runCatching { graph.api.me() }.getOrNull() ?: return@LaunchedEffect
        if (!me.signedIn) graph.settings.signOut()
    }

    NavHost(
        navController = nav,
        startDestination = start,
        modifier = Modifier.fillMaxSize().background(MaterialTheme.colorScheme.background),
    ) {
        composable<WelcomeRoute> {
            WelcomeScreen(
                onOpen = {
                    nav.navigate(HomeRoute) {
                        popUpTo<WelcomeRoute> { inclusive = true }
                        launchSingleTop = true
                    }
                },
                onSettings = { nav.navigate(SettingsRoute(frontDoor = true)) },
            )
        }
        composable<HomeRoute> {
            HomeScreen(
                onTab = onTab,
                onOpenTitle = { nav.navigate(DetailRoute(it)) },
                onPlayMovie = { nav.navigate(PlayerRoute(WatchKind.Movie, it)) },
            )
        }
        composable<BrowseRoute> { entry ->
            val route = entry.toRoute<BrowseRoute>()
            BrowseScreen(
                type = route.type,
                onTab = onTab,
                onOpenTitle = { nav.navigate(DetailRoute(it)) },
            )
        }
        composable<DetailRoute> { entry ->
            DetailScreen(
                id = entry.toRoute<DetailRoute>().id,
                onPlay = { kind, id -> nav.navigate(PlayerRoute(kind, id)) },
            )
        }
        composable<PlayerRoute> { entry ->
            val route = entry.toRoute<PlayerRoute>()
            PlayerScreen(
                kind = route.kind,
                id = route.id,
                onBack = { nav.popBackStack() },
                onSignIn = { nav.navigate(LinkRoute) },
                // "Next episode" replaces this player rather than stacking on it, so
                // Back from episode 5 goes to the series, not to episode 4.
                onPlayEpisode = { next ->
                    nav.navigate(PlayerRoute(WatchKind.Episode, next)) {
                        popUpTo<PlayerRoute> { inclusive = true }
                    }
                },
            )
        }
        composable<LinkRoute> {
            LinkScreen(onDone = { nav.popBackStack() })
        }
        composable<SettingsRoute> { entry ->
            if (entry.toRoute<SettingsRoute>().frontDoor) {
                // The pair-first screen IS the sign-in, so go back to it.
                SettingsScreen(onTab = null, onSignIn = { nav.popBackStack() })
            } else {
                SettingsScreen(onTab = onTab, onSignIn = { nav.navigate(LinkRoute) })
            }
        }
    }
}

/** Top-bar navigation: one entry per tab on the back stack, always rooted at Home. */
private fun NavHostController.openTab(tab: Tab) {
    val route: Any = when (tab) {
        Tab.Home -> HomeRoute
        Tab.Movies -> BrowseRoute("movie")
        Tab.Series -> BrowseRoute("tv")
        Tab.Search -> BrowseRoute(null)
        Tab.Account -> SettingsRoute()
    }
    navigate(route) {
        popUpTo<HomeRoute> { inclusive = route == HomeRoute }
        launchSingleTop = true
    }
}
