package online.phimnet.tv.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.navigation.NavHostController
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.rememberNavController
import androidx.navigation.toRoute
import androidx.tv.material3.MaterialTheme
import kotlinx.serialization.Serializable
import online.phimnet.tv.api.WatchKind
import online.phimnet.tv.ui.browse.BrowseScreen
import online.phimnet.tv.ui.common.Tab
import online.phimnet.tv.ui.detail.DetailScreen
import online.phimnet.tv.ui.home.HomeScreen
import online.phimnet.tv.ui.link.LinkScreen
import online.phimnet.tv.ui.player.PlayerScreen
import online.phimnet.tv.ui.settings.SettingsScreen

@Serializable data object HomeRoute

/** The discovery grid. [type] is "movie" / "tv" for the Movies / Series tabs; null is search. */
@Serializable data class BrowseRoute(val type: String? = null)

@Serializable data class DetailRoute(val id: Long)

@Serializable data class PlayerRoute(val kind: WatchKind, val id: Long)

@Serializable data object LinkRoute

@Serializable data object SettingsRoute

@Composable
fun AppNav() {
    val nav = rememberNavController()
    val onTab: (Tab) -> Unit = { nav.openTab(it) }

    NavHost(
        navController = nav,
        startDestination = HomeRoute,
        modifier = Modifier.fillMaxSize().background(MaterialTheme.colorScheme.background),
    ) {
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
        composable<SettingsRoute> {
            SettingsScreen(onTab = onTab, onSignIn = { nav.navigate(LinkRoute) })
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
        Tab.Account -> SettingsRoute
    }
    navigate(route) {
        popUpTo<HomeRoute> { inclusive = route == HomeRoute }
        launchSingleTop = true
    }
}
