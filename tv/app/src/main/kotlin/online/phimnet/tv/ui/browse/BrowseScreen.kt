package online.phimnet.tv.ui.browse

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.grid.rememberLazyGridState
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.derivedStateOf
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.focusRestorer
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.unit.dp
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.Text
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.first
import online.phimnet.tv.LocalGraph
import online.phimnet.tv.R
import online.phimnet.tv.api.Card
import online.phimnet.tv.ui.common.ChoiceChip
import online.phimnet.tv.ui.common.ErrorBox
import online.phimnet.tv.ui.common.Load
import online.phimnet.tv.ui.common.LoadingBox
import online.phimnet.tv.ui.common.PosterCard
import online.phimnet.tv.ui.common.PosterWidth
import online.phimnet.tv.ui.common.Tab
import online.phimnet.tv.ui.common.TopBar
import online.phimnet.tv.ui.common.dpadLeavesTextField
import online.phimnet.tv.ui.common.errorText
import online.phimnet.tv.ui.common.rememberLoad
import online.phimnet.tv.ui.common.rememberSessionKey
import online.phimnet.tv.ui.theme.Surface2
import online.phimnet.tv.ui.theme.TextMuted

/**
 * The discovery grid behind the Movies, Series and Search tabs. Same filter the
 * website's grid uses (text, genre, type), paged by the server 60 at a time.
 */
@Composable
fun BrowseScreen(type: String?, onTab: (Tab) -> Unit, onOpenTitle: (Long) -> Unit) {
    val graph = LocalGraph.current
    val session = rememberSessionKey()
    val isSearch = type == null

    var typed by rememberSaveable { mutableStateOf("") }
    var query by rememberSaveable { mutableStateOf("") }
    var genre by rememberSaveable { mutableStateOf<Int?>(null) }
    val genres = rememberLoad(session) { graph.api.genres() }

    // Search as they type, once they pause: typing on a TV keyboard is slow, and
    // a request per letter would reshuffle the grid under the cursor.
    LaunchedEffect(typed) {
        delay(600)
        query = typed
    }

    Column(Modifier.fillMaxSize()) {
        TopBar(
            current = when (type) {
                "movie" -> Tab.Movies
                "tv" -> Tab.Series
                else -> Tab.Search
            },
            onSelect = onTab,
            // Search has a text field that would grab focus and raise the
            // keyboard uninvited; start on the tab and let the person choose.
            focusCurrent = true,
        )
        if (isSearch) {
            SearchField(typed, onChange = { typed = it }, onSubmit = { query = typed })
        }
        (genres.state as? Load.Ready)?.value?.takeIf { it.isNotEmpty() }?.let { list ->
            LazyRow(
                modifier = Modifier.focusRestorer(),
                contentPadding = PaddingValues(horizontal = 48.dp, vertical = 12.dp),
                horizontalArrangement = Arrangement.spacedBy(10.dp),
            ) {
                item(key = "all") {
                    ChoiceChip(selected = genre == null, onClick = { genre = null }) {
                        Text(stringResource(R.string.search_all_genres))
                    }
                }
                items(list, key = { it.id }) { g ->
                    ChoiceChip(selected = genre == g.id, onClick = { genre = if (genre == g.id) null else g.id }) {
                        Text(g.name)
                    }
                }
            }
        }
        TitleGrid(
            loadKey = listOf(session, type, query, genre),
            load = { page -> graph.api.titles(query = query, genre = genre, type = type, page = page) },
            onOpenTitle = onOpenTitle,
        )
    }
}

@Composable
private fun SearchField(value: String, onChange: (String) -> Unit, onSubmit: () -> Unit) {
    var focused by remember { mutableStateOf(false) }
    Box(
        Modifier
            .padding(horizontal = 48.dp, vertical = 8.dp)
            .fillMaxWidth()
            .background(Surface2, RoundedCornerShape(8.dp))
            .border(BorderStroke(2.dp, if (focused) Color.White else Color.Transparent), RoundedCornerShape(8.dp))
            .padding(horizontal = 20.dp, vertical = 14.dp),
    ) {
        BasicTextField(
            value = value,
            onValueChange = onChange,
            singleLine = true,
            textStyle = MaterialTheme.typography.titleMedium.copy(color = Color.White),
            cursorBrush = SolidColor(Color.White),
            keyboardOptions = KeyboardOptions(imeAction = ImeAction.Search),
            keyboardActions = KeyboardActions(onSearch = { onSubmit() }),
            modifier = Modifier.fillMaxWidth().dpadLeavesTextField().onFocusChanged { focused = it.isFocused },
            decorationBox = { inner ->
                if (value.isEmpty()) Text(stringResource(R.string.search_hint), color = TextMuted)
                inner()
            },
        )
    }
}

/** A grid that fetches the next page as the focus nears the end of what is loaded. */
@Composable
private fun TitleGrid(
    loadKey: Any,
    load: suspend (page: Int) -> online.phimnet.tv.api.TitlePage,
    onOpenTitle: (Long) -> Unit,
) {
    var titles by remember(loadKey) { mutableStateOf<List<Card>>(emptyList()) }
    var total by remember(loadKey) { mutableIntStateOf(-1) }
    var page by remember(loadKey) { mutableIntStateOf(0) }
    var error by remember(loadKey) { mutableStateOf<Throwable?>(null) }
    var attempt by remember(loadKey) { mutableIntStateOf(0) }
    val grid = rememberLazyGridState()

    val wantMore by remember(loadKey) {
        derivedStateOf {
            val last = grid.layoutInfo.visibleItemsInfo.lastOrNull()?.index ?: 0
            total < 0 || (titles.size < total && last >= titles.size - 12)
        }
    }

    LaunchedEffect(loadKey, attempt) {
        // An explicit loop rather than collecting the flow: after a page lands,
        // `wantMore` may still be true (a short page, a tall screen), and a
        // distinct-until-changed collector would then never ask for the next one.
        while (true) {
            snapshotFlow { wantMore }.first { it }
            val next = try {
                load(page + 1)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                error = e
                break // retry() bumps `attempt`, which restarts this effect
            }
            // De-duplicate across pages: the list is newest-first, so a title
            // imported between two page fetches shifts everything by one.
            val seen = titles.mapTo(HashSet()) { it.id }
            titles = titles + next.titles.filter { it.id !in seen }
            total = next.total
            page = next.page
            error = null
            if (next.titles.isEmpty() || titles.size >= total) break
        }
    }

    when {
        error != null && titles.isEmpty() -> ErrorBox(errorText(error!!), onRetry = { error = null; attempt++ })
        total < 0 -> LoadingBox()
        titles.isEmpty() -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
            Text(stringResource(R.string.search_empty), color = TextMuted)
        }
        else -> LazyVerticalGrid(
            state = grid,
            columns = GridCells.Adaptive(PosterWidth),
            contentPadding = PaddingValues(horizontal = 48.dp, vertical = 16.dp),
            horizontalArrangement = Arrangement.spacedBy(20.dp),
            verticalArrangement = Arrangement.spacedBy(24.dp),
            modifier = Modifier.focusRestorer(),
        ) {
            items(titles, key = { it.id }) { card ->
                PosterCard(card, onClick = { onOpenTitle(card.id) }, modifier = Modifier.width(PosterWidth))
            }
        }
    }
}
