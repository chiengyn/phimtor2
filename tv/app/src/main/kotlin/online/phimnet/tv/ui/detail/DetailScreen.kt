package online.phimnet.tv.ui.detail

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.focusRestorer
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.tv.material3.Button
import androidx.tv.material3.Card
import androidx.tv.material3.CardDefaults
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.Text
import coil3.compose.AsyncImage
import online.phimnet.tv.LocalGraph
import online.phimnet.tv.R
import online.phimnet.tv.api.Episode
import online.phimnet.tv.api.Title
import online.phimnet.tv.api.WatchKind
import online.phimnet.tv.ui.common.ChoiceChip
import online.phimnet.tv.ui.common.ErrorBox
import online.phimnet.tv.ui.common.Load
import online.phimnet.tv.ui.common.LoadingBox
import online.phimnet.tv.ui.common.MetaLine
import online.phimnet.tv.ui.common.errorText
import online.phimnet.tv.ui.common.focusWhenReady
import online.phimnet.tv.ui.common.rememberLoad
import online.phimnet.tv.ui.common.rememberSessionKey
import online.phimnet.tv.ui.theme.Background
import online.phimnet.tv.ui.theme.Surface2
import online.phimnet.tv.ui.theme.TextMuted

@Composable
fun DetailScreen(id: Long, onPlay: (WatchKind, Long) -> Unit) {
    val graph = LocalGraph.current
    val title = rememberLoad(rememberSessionKey(), id) { graph.api.title(id) }
    when (val state = title.state) {
        Load.Loading -> LoadingBox()
        is Load.Failed -> ErrorBox(errorText(state.error), title.retry)
        is Load.Ready -> DetailContent(state.value, onPlay)
    }
}

@Composable
private fun DetailContent(title: Title, onPlay: (WatchKind, Long) -> Unit) {
    val primary = remember { FocusRequester() }

    Box(Modifier.fillMaxSize()) {
        if (title.backdrop.isNotEmpty()) {
            AsyncImage(
                model = title.backdrop,
                contentDescription = null,
                contentScale = ContentScale.Crop,
                modifier = Modifier.fillMaxSize(),
            )
        }
        Box(
            Modifier.fillMaxSize().background(
                Brush.horizontalGradient(0f to Background, 0.6f to Background.copy(alpha = 0.75f), 1f to Color.Transparent),
            ),
        )
        Box(Modifier.fillMaxSize().background(Brush.verticalGradient(0.5f to Color.Transparent, 1f to Background)))

        Column(
            Modifier
                .fillMaxSize()
                .verticalScroll(rememberScrollState())
                .padding(vertical = 48.dp),
        ) {
            Column(Modifier.padding(horizontal = 48.dp).width(680.dp)) {
                Text(title.title, style = MaterialTheme.typography.displaySmall, maxLines = 2, overflow = TextOverflow.Ellipsis)
                if (title.originalTitle.isNotBlank() && title.originalTitle != title.title) {
                    Text(title.originalTitle, style = MaterialTheme.typography.titleMedium, color = TextMuted)
                }
                Spacer(Modifier.height(12.dp))
                MetaLine(
                    listOfNotNull(
                        title.year,
                        title.runtime?.let { stringResource(R.string.detail_minutes, it) },
                        title.voteAverage.takeIf { it > 0 }?.let { "★ %.1f".format(it) },
                        title.genres.joinToString(", ") { it.name },
                    ),
                )
                Spacer(Modifier.height(16.dp))
                Text(
                    title.overview,
                    style = MaterialTheme.typography.bodyLarge,
                    color = TextMuted,
                    maxLines = 6,
                    overflow = TextOverflow.Ellipsis,
                )
                Spacer(Modifier.height(24.dp))
                if (!title.isSeries) {
                    Button(onClick = { onPlay(WatchKind.Movie, title.id) }, modifier = Modifier.focusRequester(primary)) {
                        Text(stringResource(R.string.action_watch_movie))
                    }
                }
            }
            if (title.isSeries) Seasons(title, primary, onPlay)
        }
    }

    LaunchedEffect(Unit) { primary.focusWhenReady() }
}

/** A season picker above a row of that season's episodes, as on the website. */
@Composable
private fun Seasons(title: Title, firstEpisode: FocusRequester, onPlay: (WatchKind, Long) -> Unit) {
    val seasons = title.seasons.filter { it.episodes.isNotEmpty() }
    if (seasons.isEmpty()) return
    var selected by rememberSaveable { mutableIntStateOf(0) }
    val season = seasons[selected.coerceIn(seasons.indices)]

    if (seasons.size > 1) {
        LazyRow(
            modifier = Modifier.focusRestorer(),
            contentPadding = PaddingValues(horizontal = 48.dp, vertical = 8.dp),
            horizontalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            items(seasons.size) { i ->
                ChoiceChip(selected = i == selected, onClick = { selected = i }) {
                    Text(seasons[i].name.ifBlank { stringResource(R.string.detail_season, seasons[i].number) })
                }
            }
        }
    }
    LazyRow(
        modifier = Modifier.focusRestorer(),
        contentPadding = PaddingValues(horizontal = 48.dp, vertical = 16.dp),
        horizontalArrangement = Arrangement.spacedBy(20.dp),
    ) {
        items(season.episodes, key = { it.id }) { ep ->
            EpisodeCard(
                ep,
                modifier = if (ep == season.episodes.first()) Modifier.focusRequester(firstEpisode) else Modifier,
                onClick = { onPlay(WatchKind.Episode, ep.id) },
            )
        }
    }
}

@Composable
private fun EpisodeCard(ep: Episode, modifier: Modifier, onClick: () -> Unit) {
    Column(modifier.width(260.dp)) {
        Card(
            onClick = onClick,
            modifier = Modifier.size(width = 260.dp, height = 146.dp),
            shape = CardDefaults.shape(RoundedCornerShape(6.dp)),
            scale = CardDefaults.scale(focusedScale = 1.06f),
        ) {
            Box(Modifier.fillMaxSize().background(Surface2), contentAlignment = Alignment.Center) {
                if (ep.still.isNotEmpty()) {
                    AsyncImage(model = ep.still, contentDescription = ep.name, contentScale = ContentScale.Crop, modifier = Modifier.fillMaxSize())
                } else {
                    Text(stringResource(R.string.detail_episode, ep.number), style = MaterialTheme.typography.titleMedium)
                }
            }
        }
        Spacer(Modifier.height(8.dp))
        Text(
            "${ep.number}. ${ep.name.ifBlank { stringResource(R.string.detail_episode, ep.number) }}",
            style = MaterialTheme.typography.bodyMedium,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier.fillMaxWidth(),
        )
        ep.runtime?.let {
            Text(stringResource(R.string.detail_minutes, it), style = MaterialTheme.typography.labelSmall, color = TextMuted)
        }
    }
}
