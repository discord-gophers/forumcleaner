package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/gateway"
	"github.com/diamondburned/arikawa/v3/state"
	"github.com/diamondburned/arikawa/v3/utils/json/option"
	"github.com/pkg/errors"
	"golang.org/x/exp/slices"
)

const (
	GCInterval = time.Minute

	SolvedTimeout = time.Minute
	SolvedTag     = "solved"

	Day = 24 * time.Hour

	StaleTag = "stale"
	// inactivity timeout before applying stale tag
	StaleTimeout = 1 * Day
	// how long to wait before closing a stale post
	StaleGracePeriod = 1 * Day

	HerderRole = 370280974593818644
	SgtTailor  = 189020382559207425
)

var solvedCommandID = ""

func main() {
	log.SetFlags(0)

	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		log.Fatalln("No $BOT_TOKEN given.")
	}

	var b Bot

	b.s = state.New("Bot " + token)
	b.s.AddInteractionHandler(&b)
	b.s.AddIntents(gateway.IntentGuilds)
	b.s.AddHandler(func(*gateway.ReadyEvent) {
		me, _ := b.s.Me()
		log.Println("connected to the gateway as", me.Tag())
	})

	if err := overwriteCommands(b.s); err != nil {
		log.Fatalln("cannot update commands:", err)
	}

	if err := b.s.Open(context.Background()); err != nil {
		log.Fatalln("cannot connect:", err)
	}

	go b.startGarbageCollection()

	if err := b.s.Wait(context.Background()); err != nil {
		log.Fatalln("discord gone bad:", err)
	}
}

var commands = []api.CreateCommandData{
	{
		Name:        "solved",
		Description: "marks the current forum post as resolved",
	},
	{
		Name:        "done",
		Description: "prompts to mark the forum post as resolved",
	},
}

func overwriteCommands(s *state.State) error {
	app, err := s.CurrentApplication()
	if err != nil {
		return errors.Wrap(err, "cannot get current app ID")
	}

	newCmds, err := s.BulkOverwriteCommands(app.ID, commands)
	for _, c := range newCmds {
		if c.Name == "solved" {
			solvedCommandID = c.ID.String()
		}
	}

	return err
}

type Bot struct {
	s *state.State

	// tag cache per guild. Per parent channelID (forum channel) we keep track
	// of tags that exists. Pretty cursed but it does the job
	tagCache map[discord.ChannelID]map[string]discord.TagID
}

func (b *Bot) startGarbageCollection() {
	for range time.NewTicker(GCInterval).C {
		b.collectGarbage()
	}
}

func (b *Bot) collectGarbage() {
	log.Println("garbage collecting solved threads")
	guilds, err := b.s.Guilds()
	if err != nil {
		log.Println("error fetching guilds: ", err)
		return
	}

	for _, guild := range guilds {
		b.cleanGuild(guild)
	}
}

func (b *Bot) cleanGuild(guild discord.Guild) {
	log.Println("cleaning guild", guild.ID, guild.Name)
	threads, err := b.s.ActiveThreads(guild.ID)
	if err != nil {
		log.Println("error getting active threads", err)
		return
	}

	log.Printf("%d active threads\n", len(threads.Threads))

	b.fillTagCache(threads.Threads)
	b.cleanSolvedThreads(threads.Threads)
	b.markStaleThreads(threads.Threads)
	b.cleanStaleThreads(threads.Threads)
	b.cleanPinnedThreads(threads.Threads)
}

func (b *Bot) fillTagCache(threads []discord.Channel) {
	b.tagCache = make(map[discord.ChannelID]map[string]discord.TagID)

	for _, thread := range threads {
		// cache already populated
		if _, ok := b.tagCache[thread.ParentID]; ok {
			continue
		}

		b.tagCache[thread.ParentID] = make(map[string]discord.TagID)

		parent, err := b.s.Client.Channel(thread.ParentID)
		if err != nil {
			log.Println("couldn't fetch parent channel:", err)
			continue
		}

		for _, tag := range parent.AvailableTags {
			b.tagCache[thread.ParentID][tag.Name] = tag.ID
		}
	}
}

func (b *Bot) cleanSolvedThreads(threads []discord.Channel) {
	for _, thread := range threads {
		log.Println("checking thread", thread.ID, thread.Name)
		tagID, ok := b.tagCache[thread.ParentID][SolvedTag]

		// cache miss but no solved tag exists
		if !ok {
			log.Println("skipping, no solved tag exists")
			continue
		}

		// refresh data as the api no longer returns applied_tags: https://github.com/discord/discord-api-docs/issues/6258
		thread, err := b.s.Channel(thread.ID)
		if err != nil {
			log.Printf("error getting thread: %s", err)
			return
		}

		// no solved tag set
		if !slices.Contains(thread.AppliedTags, tagID) {
			log.Println("skipping, solved tag not set")
			continue
		}

		// post hasn't expired yet
		if time.Since(thread.LastMessageID.Time()) < SolvedTimeout {
			log.Printf("skipping solved tag, post has recent activity: %s", time.Since(thread.LastMessageID.Time()))
			continue
		}

		log.Println("closing thread")

		// post has the solved tag and had no activity. Time to archive
		err = b.s.Client.ModifyChannel(thread.ID, api.ModifyChannelData{
			Archived: option.True,
		})

		if err != nil {
			log.Println("Error closing thread: ", err)
		}
	}
}

func (b *Bot) markStaleThreads(threads []discord.Channel) {
	log.Println("marking stale threads")
	for _, thread := range threads {
		log.Println("checking thread", thread.ID, thread.Name)

		if thread.Flags&discord.PinnedThread == discord.PinnedThread {
			log.Println("thread is pinned, skipping")
			continue
		}

		tags, exists := b.tagCache[thread.ParentID]
		if !exists {
			log.Println("skipping, tag cache empty, should never happen")
			continue
		}

		tagID, exists := tags[StaleTag]
		if !exists {
			log.Println("skipping, stale tag does not exist")
			continue
		}

		if time.Since(thread.LastMessageID.Time()) < StaleTimeout {
			log.Println("skpping, has recent activity")
			if index := slices.Index(thread.AppliedTags, tagID); index >= 0 {
				log.Println("stale thread has recent activity, unmarking as stale")
				newTags := append(thread.AppliedTags[:index], thread.AppliedTags[index+1:]...)
				err := b.s.Client.ModifyChannel(thread.ID, api.ModifyChannelData{
					AppliedTags: &newTags,
				})
				if err != nil {
					log.Println("error removing stale tag:", err)
				}
			}
			continue
		}

		if slices.Contains(thread.AppliedTags, tagID) {
			log.Println("skipping, already marked as stale")
			continue
		}

		newtags := append(thread.AppliedTags, tagID)
		err := b.s.Client.ModifyChannel(thread.ID, api.ModifyChannelData{
			AppliedTags: &newtags,
		})
		if err != nil {
			log.Println("error modifying channel:", err)
		}
	}
}

func (b *Bot) cleanStaleThreads(threads []discord.Channel) {
	log.Println("cleaning stale threads")
	for _, thread := range threads {
		log.Println("checking thread", thread.ID, thread.Name)

		if thread.Flags&discord.PinnedThread == discord.PinnedThread {
			log.Println("thread is pinned, skipping")
			continue
		}

		tags, exists := b.tagCache[thread.ParentID]
		if !exists {
			log.Println("skipping, tag cache empty, should never happen")
			continue
		}

		_, exists = tags[StaleTag]
		if !exists {
			log.Println("skipping, stale tag does not exist")
			continue
		}

		if time.Since(thread.LastMessageID.Time()) < StaleTimeout+StaleGracePeriod {
			log.Println("skpping, has recent activity")
			continue
		}

		err := b.s.Client.ModifyChannel(thread.ID, api.ModifyChannelData{
			Archived: option.True,
		})
		if err != nil {
			log.Println("error modifying channel:", err)
		}
	}
}

func (b *Bot) cleanPinnedThreads(threads []discord.Channel) {
	log.Println("checking for messaged in pinned thread")
	for _, thread := range threads {
		if thread.Flags&discord.PinnedThread != discord.PinnedThread {
			log.Println("skipping, thread is not pinned")
			continue
		}

		messages, err := b.s.Client.Messages(thread.ID, 0)
		if err != nil {
			log.Println("error fetching messages for pinned thread:", err)
			continue
		}

		for _, message := range messages {
			if message.ID == discord.MessageID(thread.ID) {
				continue
			}

			if message.Type != discord.DefaultMessage && message.Type != discord.InlinedReplyMessage {
				continue
			}

			log.Printf("deleting message %#v", message)
			err := b.s.Client.DeleteMessage(thread.ID, message.ID, "comment in pinned forum post")
			if err != nil {
				log.Println("error deleting message:", err)
			}
		}
	}
}

func (b *Bot) HandleInteraction(ev *discord.InteractionEvent) *api.InteractionResponse {
	switch data := ev.Data.(type) {
	case *discord.CommandInteraction:
		switch data.Name {
		case "solved":
			return b.cmdSolved(ev, data)
		case "done":
			return b.cmdDone(ev, data)
		default:
			return errorResponse(fmt.Sprintf("unknown command %q", data.Name))
		}
	case *discord.ButtonInteraction:
		switch data.CustomID {
		case "solved":
			return b.cmdSolved(ev, data)
		default:
			return errorResponse(fmt.Sprintf("unknown event %q", data.CustomID))
		}
	default:
		return errorResponse(fmt.Sprintf("unknown interaction %T", ev.Data))
	}
}

func (b *Bot) checkAllowlist(g discord.GuildID, c *discord.Channel, m *discord.Member) bool {
	if m.User.ID == SgtTailor {
		return true
	}

	if c.OwnerID == m.User.ID {
		return true
	}

	return slices.Contains(m.RoleIDs, HerderRole)
}

func (b *Bot) cmdSolved(ev *discord.InteractionEvent, data discord.InteractionData) *api.InteractionResponse {
	channel, err := b.s.Client.Channel(ev.ChannelID)
	if err != nil {
		return errorResponse(fmt.Sprintf("can't read channel: %s", err))
	}

	allowed := b.checkAllowlist(ev.GuildID, channel, ev.Member)
	if !allowed {
		return errorResponse("permission denied")
	}

	if !channel.ParentID.IsValid() {
		return errorResponse("this command only works in forum posts")
	}

	parent, err := b.s.Client.Channel(channel.ParentID)
	if err != nil {
		return errorResponse(fmt.Sprintf("can't read parent channel: %s", err))
	}

	if parent.Type != discord.GuildForum {
		return errorResponse("this command only works in forum posts")
	}

	var solvedTag *discord.Tag
	for _, tag := range parent.AvailableTags {
		if tag.Name == SolvedTag {
			solvedTag = &tag
			break
		}
	}

	if solvedTag == nil {
		return errorResponse("no solved tag found to apply")
	}

	if slices.Contains(channel.AppliedTags, solvedTag.ID) {
		return errorResponse("post already marked as solved")
	}

	newtags := append(channel.AppliedTags, solvedTag.ID)
	err = b.s.Client.ModifyChannel(ev.ChannelID, api.ModifyChannelData{
		AppliedTags: &newtags,
	})

	if err != nil {
		return errorResponse(fmt.Sprintf("error applying tag: %s", err))
	}

	return &api.InteractionResponse{
		Type: api.MessageInteractionWithSource,
		Data: &api.InteractionResponseData{
			Content:         option.NewNullableString("Thread marked as solved"),
			Flags:           discord.EphemeralMessage,
			AllowedMentions: &api.AllowedMentions{},
		},
	}
}

func (b *Bot) cmdDone(ev *discord.InteractionEvent, _ discord.InteractionData) *api.InteractionResponse {
	channel, err := b.s.Client.Channel(ev.ChannelID)
	if err != nil {
		return errorResponse(fmt.Sprintf("can't read channel: %s", err))
	}

	if !channel.ParentID.IsValid() {
		return errorResponse("this command only works in forum posts")
	}

	parent, err := b.s.Client.Channel(channel.ParentID)
	if err != nil {
		return errorResponse(fmt.Sprintf("can't read parent channel: %s", err))
	}

	if parent.Type != discord.GuildForum {
		return errorResponse("this command only works in forum posts")
	}

	var solvedTag *discord.Tag
	for _, tag := range parent.AvailableTags {
		if tag.Name == SolvedTag {
			solvedTag = &tag
			break
		}
	}

	if solvedTag == nil {
		return errorResponse("no solved tag found to apply")
	}

	if slices.Contains(channel.AppliedTags, solvedTag.ID) {
		return errorResponse("post already marked as solved")
	}

	return &api.InteractionResponse{
		Type: api.MessageInteractionWithSource,
		Data: &api.InteractionResponseData{
			Content: option.NewNullableString(fmt.Sprintf(
				"If your question has been solved, please close the thread with </solved:%s>",
				solvedCommandID,
			)),
			Components: &discord.ContainerComponents{
				&discord.ActionRowComponent{
					&discord.ButtonComponent{
						CustomID: "solved",
						Label:    "Mark as Solved",
						Style:    discord.SuccessButtonStyle(),
					},
				},
			},
			AllowedMentions: &api.AllowedMentions{},
		},
	}
}

func errorResponse(err string) *api.InteractionResponse {
	return &api.InteractionResponse{
		Type: api.MessageInteractionWithSource,
		Data: &api.InteractionResponseData{
			Content:         option.NewNullableString(err),
			Flags:           discord.EphemeralMessage,
			AllowedMentions: &api.AllowedMentions{ /* none */ },
		},
	}
}
