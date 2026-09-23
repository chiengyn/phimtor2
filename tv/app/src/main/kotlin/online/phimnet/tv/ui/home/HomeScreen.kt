package online.phimnet.tv.ui.home

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.withFrameNanos
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.focusRestorer
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.tv.material3.Button
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.OutlinedButton
import androidx.tv.material3.Text
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import coil3.compose.AsyncImage
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.first
import online.phimnet.tv.LocalGraph
import online.phimnet.tv.R
import online.phimnet.tv.api.Home
import online.phimnet.tv.api.Title
import online.phimnet.tv.ui.common.ErrorBox
import online.phimnet.tv.ui.common.Load
import online.phimnet.tv.ui.common.LoadingBox
import online.phimnet.tv.ui.common.MetaLine
import online.phimnet.tv.ui.common.PosterCard
import online.phimnet.tv.ui.common.Tab
import online.phimnet.tv.ui.common.TopBar
import online.phimnet.tv.ui.common.UpdatePanel
import online.phimnet.tv.ui.common.isActionable
import online.phimnet.tv.ui.common.errorText
import online.phimnet.tv.ui.common.focusWhenReady
import online.phimnet.tv.ui.common.rememberLoad
import online.phimnet.tv.ui.common.rememberSessionKey
import online.phimnet.tv.ui.theme.Background
import online.phimnet.tv.ui.theme.TextMuted

@Composable
fun HomeScreen(
    onTab: (Tab) -> Unit,
    onOpenTitle: (Long) -> Unit,
    onPlayMovie: (Long) -> Unit,
) {
    val graph = LocalGraph.current
    val home = rememberLoad(rememberSessionKey()) { graph.api.home() }

    val update by graph.updater.state.collectAsStateWithLifecycle()
    val updateDismissed by graph.updater.dismissed.collectAsStateWithLifecycle()
    // "Later" removes the banner together with its focused button; the hero takes
    // focus back — but only when it was dismissed on this visit, so coming back
    // to Home from a title still returns to the card that was opened.
    val dismissedOnEntry = remember { updateDismissed }

    Column(Modifier.fillMaxSize()) {
        TopBar(current = Tab.Home, onSelect = onTab)
        // A sideloaded app has no store to update it. The offer is pinned under
        // the top bar, NOT an item in the list below: as a list item, focusing the
        // hero's Play on launch scrolled it straight off the top, so nobody saw it
        // (found on the emulator). It never takes the initial focus — someone who
        // opened the app to watch should not land on an update button — and is one
        // press of ↑ away.
        if (!updateDismissed && update.isActionable()) {
            UpdatePanel(update, inSettings = false, modifier = Modifier.padding(bottom = 12.dp))
        }
        when (val state = home.state) {
            Load.Loading -> LoadingBox()
            is Load.Failed -> ErrorBox(errorText(state.error), home.retry)
            is Load.Ready -> HomeContent(
                state.value,
                onOpenTitle,
                onPlayMovie,
                compactHero = !updateDismissed && update.isActionable(),
                focusHero = updateDismissed && !dismissedOnEntry,
            )
        }
    }
}

@Composable
private fun HomeContent(
    home: Home,
    onOpenTitle: (Long) -> Unit,
    onPlayMovie: (Long) -> Unit,
    // With the update banner pinned above it the list is shorter; a compact hero
    // keeps the first row peeking at the bottom, which is what tells someone the
    // page continues.
    compactHero: Boolean = false,
    focusHero: Boolean = false,
) {
    if (home.rows.isEmpty() && home.hero.isEmpty()) {
        Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
            Text(stringResource(R.string.home_empty), color = TextMuted)
        }
        return
    }

    // Which card was opened last, so Back returns focus to it instead of to the
    // top of the page. Saved across the trip to the detail screen and back.
    var lastOpened by rememberSaveable { mutableStateOf<String?>(null) }

    val heroFocus = remember { FocusRequester() }
    val listState = rememberLazyListState()

    LazyColumn(
        state = listState,
        contentPadding = PaddingValues(bottom = 48.dp),
        verticalArrangement = Arrangement.spacedBy(28.dp),
    ) {
        if (home.hero.isNotEmpty()) {
            item(key = "hero") {
                Hero(
                    height = if (compactHero) 330.dp else 420.dp,
                    titles = home.hero,
                    playFocus = heroFocus,
                    onPlay = { t -> if (t.isSeries) onOpenTitle(t.id) else onPlayMovie(t.id) },
                    onDetails = { t -> onOpenTitle(t.id) },
                )
            }
        }
        home.rows.forEachIndexed { rowIndex, row ->
            item(key = "row:${row.key}:$rowIndex") {
                Column {
                    Text(
                        row.label,
                        modifier = Modifier.padding(start = 48.dp, bottom = 12.dp),
                        style = MaterialTheme.typography.titleLarge,
                    )
                    LazyRow(
                        // Re-entering a row lands on the card last focused in it,
                        // not always on the first.
                        modifier = Modifier.focusRestorer(),
                        contentPadding = PaddingValues(horizontal = 48.dp, vertical = 8.dp),
                        horizontalArrangement = Arrangement.spacedBy(20.dp),
                    ) {
                        itemsIndexed(row.titles, key = { _, card -> card.id }) { index, card ->
                            val key = "$rowIndex:${card.id}"
                            val focus = remember { FocusRequester() }
                            PosterCard(
                                card = card,
                                rank = if (row.ranked) index + 1 else null,
                                modifier = Modifier.focusRequester(focus),
                                onClick = {
                                    lastOpened = key
                                    onOpenTitle(card.id)
                                },
                            )
                            if (key == lastOpened) {
                                LaunchedEffect(Unit) { focus.focusWhenReady() }
                            }
                        }
                    }
                }
            }
        }
    }

    // First visit: start on the hero's Play button, the most likely next press —
    // then put the list back at the top. On a TV, Compose brings a focused item
    // into view with a "pivot" scroll (to a fixed fraction of the viewport) even
    // when it was already fully visible; under the pinned update banner that
    // shoved the hero's title out of sight. The pivot only runs on focus changes,
    // so resetting once afterwards sticks.
    LaunchedEffect(Unit) {
        if (lastOpened == null && home.hero.isNotEmpty()) {
            heroFocus.focusWhenReady()
            listState.scrollToItem(0)
        }
    }
    // The banner usually arrives AFTER Home has laid out — the update check and
    // the catalog load race — and its arrival shrinks this list from the top. The
    // TV pivot scroll then keeps the focused Play at its line by pushing the hero's
    // title out of view (seen on the emulator). So when the banner comes or goes,
    // let that resize and its scroll settle, then return to the top — but only if
    // the person is still on the hero, never yanking them out of a row.
    LaunchedEffect(focusHero) {
        if (focusHero && home.hero.isNotEmpty()) heroFocus.focusWhenReady()
    }
    LaunchedEffect(compactHero) {
        repeat(2) { withFrameNanos { } }
        snapshotFlow { listState.isScrollInProgress }.first { !it }
        if (listState.firstVisibleItemIndex == 0) listState.animateScrollToItem(0)
    }
}

/**
 * The billboard: the admin's curated featured titles, the same list and order
 * as the website's hero. It advances on its own — but never while it has focus,
 * because changing the title under a focused Play button would play something
 * the person did not choose.
 */
@Composable
private fun Hero(
    height: Dp,
    titles: List<Title>,
    playFocus: FocusRequester,
    onPlay: (Title) -> Unit,
    onDetails: (Title) -> Unit,
) {
    var index by rememberSaveable { mutableIntStateOf(0) }
    var focused by remember { mutableStateOf(false) }
    val title = titles[index % titles.size]

    LaunchedEffect(focused, titles.size) {
        if (titles.size < 2) return@LaunchedEffect
        while (!focused) {
            delay(9_000)
            if (!focused) index = (index + 1) % titles.size
        }
    }

    Box(
        Modifier
            .fillMaxWidth()
            .height(height)
            .onFocusChanged { focused = it.hasFocus },
    ) {
        if (title.backdrop.isNotEmpty()) {
            AsyncImage(
                model = title.backdrop,
                contentDescription = null,
                contentScale = ContentScale.Crop,
                modifier = Modifier.fillMaxSize(),
            )
        }
        // Scrims: text must stay legible over any backdrop.
        Box(
            Modifier.fillMaxSize().background(
                Brush.horizontalGradient(0f to Background, 0.55f to Background.copy(alpha = 0.6f), 1f to Color.Transparent),
            ),
        )
        Box(
            Modifier.fillMaxSize().background(
                Brush.verticalGradient(0.6f to Color.Transparent, 1f to Background),
            ),
        )
        Column(
            Modifier.fillMaxHeight().width(620.dp).padding(start = 48.dp, top = 24.dp),
            verticalArrangement = Arrangement.Center,
        ) {
            Text(
                title.title,
                style = MaterialTheme.typography.displaySmall,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
            )
            Spacer(Modifier.height(8.dp))
            MetaLine(
                listOfNotNull(
                    title.year,
                    title.voteAverage.takeIf { it > 0 }?.let { "★ %.1f".format(it) },
                    title.genres.take(3).joinToString(", ") { it.name },
                ),
            )
            Spacer(Modifier.height(12.dp))
            Text(
                title.overview,
                maxLines = 3,
                overflow = TextOverflow.Ellipsis,
                style = MaterialTheme.typography.bodyLarge,
                color = TextMuted,
            )
            Spacer(Modifier.height(20.dp))
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                Button(onClick = { onPlay(title) }, modifier = Modifier.focusRequester(playFocus)) {
                    Text(stringResource(R.string.action_play))
                }
                OutlinedButton(onClick = { onDetails(title) }) {
                    Text(stringResource(R.string.action_details))
                }
            }
        }
    }
}
