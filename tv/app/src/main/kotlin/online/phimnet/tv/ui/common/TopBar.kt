package online.phimnet.tv.ui.common

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.remember
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.tv.material3.ClickableSurfaceDefaults
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.Surface
import androidx.tv.material3.Text
import online.phimnet.tv.R
import online.phimnet.tv.ui.theme.Accent
import online.phimnet.tv.ui.theme.TextMuted

enum class Tab { Home, Movies, Series, Search, Account }

/**
 * The top navigation, mirroring the website's header: home, films, series,
 * search, account. The current tab is marked but still focusable, so pressing
 * up from any row always lands somewhere.
 */
@Composable
fun TopBar(
    current: Tab,
    onSelect: (Tab) -> Unit,
    modifier: Modifier = Modifier,
    /**
     * Start with focus on the current tab. Tab screens reached from this bar set
     * it, so left/right keeps walking the tabs; otherwise focus would land on
     * the FIRST tab, "Home", after every switch. Home itself leaves it off and
     * focuses its hero instead.
     */
    focusCurrent: Boolean = false,
) {
    val currentFocus = remember { FocusRequester() }
    if (focusCurrent) LaunchedEffect(Unit) { currentFocus.focusWhenReady() }
    Row(
        modifier.fillMaxWidth().padding(start = 48.dp, end = 48.dp, top = 24.dp, bottom = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        Text(
            "phimnet",
            style = MaterialTheme.typography.headlineSmall.copy(fontWeight = FontWeight.Black),
            color = Accent,
        )
        Spacer(Modifier.width(24.dp))
        for (tab in Tab.entries) {
            val selected = tab == current
            Surface(
                onClick = { onSelect(tab) },
                modifier = if (selected) Modifier.focusRequester(currentFocus) else Modifier,
                shape = ClickableSurfaceDefaults.shape(MaterialTheme.shapes.small),
                colors = ClickableSurfaceDefaults.colors(
                    containerColor = androidx.compose.ui.graphics.Color.Transparent,
                    contentColor = if (selected) MaterialTheme.colorScheme.onSurface else TextMuted,
                    focusedContainerColor = MaterialTheme.colorScheme.onSurface,
                    focusedContentColor = MaterialTheme.colorScheme.surface,
                ),
                scale = ClickableSurfaceDefaults.scale(focusedScale = 1f),
            ) {
                Text(
                    stringResource(tab.label),
                    modifier = Modifier.padding(horizontal = 14.dp, vertical = 6.dp),
                    style = MaterialTheme.typography.titleSmall.copy(
                        fontWeight = if (selected) FontWeight.Bold else FontWeight.Normal,
                    ),
                )
            }
        }
    }
}

private val Tab.label: Int
    get() = when (this) {
        Tab.Home -> R.string.nav_home
        Tab.Movies -> R.string.nav_movies
        Tab.Series -> R.string.nav_series
        Tab.Search -> R.string.nav_search
        Tab.Account -> R.string.nav_account
    }
